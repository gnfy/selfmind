package envprofiles

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// copyInResult reports what a bounded copy actually moved.
type copyInResult struct {
	Files int
	Bytes int64
}

// runCopyIn materializes host state into dest.
//
// Bounds are hard: exceeding MaxBytes, MaxFiles or MaxDepth fails the copy
// instead of producing a partially populated credential store, which would look
// like a corrupt login rather than a configuration problem. The copy lands in a
// staging directory and is renamed into place, so a crash mid-copy can never
// leave a half-written state overlay that the next command would trust.
//
// Copying is once per run: if dest already exists the copy is skipped, which
// also preserves an access token the tool refreshed into its own overlay.
func runCopyIn(spec CopyIn, source, dest string) (copyInResult, bool, error) {
	var result copyInResult
	if source == "" {
		return result, false, nil
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		return result, false, nil // already materialized for this run
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing to copy is not an error: the tool will report its own
			// "not configured" state, which is the honest diagnosis.
			return result, false, nil
		}
		return result, false, fmt.Errorf("inspect %s: %w", filepath.Base(source), err)
	}
	if !sourceInfo.IsDir() {
		return result, false, fmt.Errorf("copy_in source must be a directory")
	}
	if len(spec.Include) == 0 {
		return result, false, fmt.Errorf("copy_in requires an explicit include list")
	}
	if spec.MaxBytes <= 0 || spec.MaxFiles <= 0 || spec.MaxDepth <= 0 {
		return result, false, fmt.Errorf("copy_in requires positive size, file and depth bounds")
	}

	staging := dest + ".staging"
	if err := os.RemoveAll(staging); err != nil {
		return result, false, err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return result, false, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()

	walkErr := filepath.WalkDir(source, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		rel, relErr := filepath.Rel(source, current)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		depth := len(strings.Split(slashRel, "/"))
		if depth > spec.MaxDepth {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if matchesAny(slashRel, spec.Exclude) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			// Descend only when the directory itself is included or could
			// contain an included path.
			if matchesAny(slashRel, spec.Include) || prefixCouldMatch(slashRel, spec.Include) {
				return nil
			}
			return fs.SkipDir
		}
		if !matchesAny(slashRel, spec.Include) {
			return nil
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		// Only regular files are copied. A symlink could point outside the
		// source tree, and a device, socket or FIFO has no meaning in a copied
		// credential store — all are refused rather than followed.
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to copy non-regular file %s", slashRel)
		}
		result.Files++
		if result.Files > spec.MaxFiles {
			return fmt.Errorf("copy_in exceeded its %d-file bound at %s", spec.MaxFiles, slashRel)
		}
		result.Bytes += info.Size()
		if result.Bytes > spec.MaxBytes {
			return fmt.Errorf("copy_in exceeded its %d-byte bound at %s (a state directory this large is a configuration problem, not a copy to widen)",
				spec.MaxBytes, slashRel)
		}
		target := filepath.Join(staging, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyRegularFile(current, target, info.Mode().Perm())
	})
	if walkErr != nil {
		return result, false, walkErr
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return result, false, err
	}
	// Atomic publish: staging and dest are siblings, so this is a rename within
	// one filesystem.
	if err := os.Rename(staging, dest); err != nil {
		return result, false, fmt.Errorf("publish state overlay: %w", err)
	}
	cleanup = false
	return result, true, nil
}

// copyRegularFile copies content and preserves the permission bits: a copied
// credentials file must stay 0600, not inherit the umask.
func copyRegularFile(source, dest string, perm os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dest, perm)
}

func matchesAny(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchGlob(pattern, rel) {
			return true
		}
	}
	return false
}

// matchGlob supports the two shapes the catalog uses: a plain path.Match
// pattern, and a trailing "/**" meaning "this directory and everything under
// it". Keeping the syntax this small avoids a second pattern language.
func matchGlob(pattern, rel string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return rel == prefix || strings.HasPrefix(rel, prefix+"/")
	}
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	// A pattern without a slash also matches at the top level only, which
	// path.Match already handles; nested files must be named explicitly.
	return false
}

// prefixCouldMatch reports whether descending into dir could still reach an
// included path, so the walk does not skip parents of included files.
func prefixCouldMatch(dir string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(strings.TrimSuffix(pattern, "/**"))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, dir+"/") {
			return true
		}
	}
	return false
}
