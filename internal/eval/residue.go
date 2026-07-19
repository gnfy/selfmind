package eval

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// On-disk eval residue: before the SkillsDir isolation fix every eval case
// minted a `<home>/.selfmind/eval-<case>-<nano>/skills` tree (app wiring
// defaults the skills base dir to the home dir), and historic shared-data runs
// left `eval-<case>-<nano>/memory.db` tenant dirs under the configured data
// dir. This file removes exactly that residue and nothing else. There is
// deliberately NO generalized recursive delete: a directory qualifies only if
// its name matches the eval tenant pattern, it is a DIRECT child of a known
// root, and every entry inside it is a known eval artifact. One unrecognized
// entry disqualifies the whole directory — it is reported and skipped.

// evalResidueDirName matches the throwaway eval tenant IDs the harness mints:
// `eval-<sanitized-case-id>-<unix-nanos>` (nanosecond timestamps are 19 digits
// today; the range guards against clock oddities without loosening the shape).
var evalResidueDirName = regexp.MustCompile(`^eval-[A-Za-z0-9_]+-[0-9]{15,20}$`)

// evalMemoryFiles are the per-tenant memory store files the SQLite provider
// creates inside a tenant dir under the data dir.
var evalMemoryFiles = map[string]bool{
	"memory.db":     true,
	"memory.db-wal": true,
	"memory.db-shm": true,
}

// skillFileExts are the only file types skill tooling writes under a tenant
// `skills/` tree. Any other file type marks the directory as not-ours.
var skillFileExts = map[string]bool{
	".md":   true,
	".yaml": true,
	".yml":  true,
	".json": true,
	".txt":  true,
}

// DiskResidueDir is one qualifying residue directory.
type DiskResidueDir struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// DiskResidueSkip is a matching-name directory that was NOT removed, with the
// reason (foreign contents, unreadable, or a failed delete).
type DiskResidueSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DiskResidueRoot reports one scanned root.
type DiskResidueRoot struct {
	Root       string            `json:"root"`
	Candidates []DiskResidueDir  `json:"candidates,omitempty"`
	Skipped    []DiskResidueSkip `json:"skipped,omitempty"`
	Bytes      int64             `json:"bytes"`
}

// DiskResidueReport aggregates the scan (dry run) or deletion (apply) result.
type DiskResidueReport struct {
	Roots []DiskResidueRoot `json:"roots"`
	// Removed counts directories actually deleted; always 0 on a dry run.
	Removed int `json:"removed"`
}

// TotalDirs returns the number of qualifying residue directories found (and,
// with apply, removed).
func (r *DiskResidueReport) TotalDirs() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, root := range r.Roots {
		n += len(root.Candidates)
	}
	return n
}

// TotalBytes returns the summed content size of all qualifying directories.
func (r *DiskResidueReport) TotalBytes() int64 {
	if r == nil {
		return 0
	}
	var b int64
	for _, root := range r.Roots {
		b += root.Bytes
	}
	return b
}

// TotalSkipped returns the number of matching-name directories left untouched.
func (r *DiskResidueReport) TotalSkipped() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, root := range r.Roots {
		n += len(root.Skipped)
	}
	return n
}

// Empty reports whether the scan found nothing at all.
func (r *DiskResidueReport) Empty() bool {
	return r.TotalDirs() == 0 && r.TotalSkipped() == 0
}

// CleanDiskResidue scans the given roots (typically the config home dir and
// the storage data dir) for eval residue directories. With apply=false nothing
// is touched and the report describes what WOULD be removed; with apply=true
// each qualifying directory is deleted and failures are reported as skips.
// Missing or unreadable roots simply contribute no residue.
func CleanDiskResidue(roots []string, apply bool) *DiskResidueReport {
	report := &DiskResidueReport{}
	seen := map[string]bool{}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		rr := DiskResidueRoot{Root: root}
		entries, err := os.ReadDir(root)
		if err != nil {
			report.Roots = append(report.Roots, rr)
			continue
		}
		for _, entry := range entries {
			// DirEntry.IsDir uses lstat semantics, so a symlink named like a
			// residue dir never qualifies.
			if !entry.IsDir() || !evalResidueDirName.MatchString(entry.Name()) {
				continue
			}
			path := filepath.Join(root, entry.Name())
			size, reason := classifyEvalResidueDir(path)
			if reason != "" {
				rr.Skipped = append(rr.Skipped, DiskResidueSkip{Path: path, Reason: reason})
				continue
			}
			if apply {
				if err := os.RemoveAll(path); err != nil {
					rr.Skipped = append(rr.Skipped, DiskResidueSkip{Path: path, Reason: "delete failed: " + err.Error()})
					continue
				}
				report.Removed++
			}
			rr.Candidates = append(rr.Candidates, DiskResidueDir{Path: path, Bytes: size})
			rr.Bytes += size
		}
		report.Roots = append(report.Roots, rr)
	}
	return report
}

// RemoveEvalTenantDir deletes one specific `<baseDir>/<tenantID>` directory a
// finished eval harness minted for its own throwaway tenant. Home-anchored
// skill tooling keys per-tenant skill roots under ~/.selfmind regardless of
// config, so a skills-dir config override alone cannot stop that directory
// from appearing; the harness calls this on Close to guarantee no per-case
// residue survives. The same strict rules apply as for the batch cleaner: the
// tenant ID must match the eval pattern and the directory must contain only
// known eval artifacts — anything else is left in place.
func RemoveEvalTenantDir(baseDir, tenantID string) bool {
	if strings.TrimSpace(baseDir) == "" || !evalResidueDirName.MatchString(tenantID) {
		return false
	}
	path := filepath.Join(baseDir, tenantID)
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if _, reason := classifyEvalResidueDir(path); reason != "" {
		return false
	}
	return os.RemoveAll(path) == nil
}

// classifyEvalResidueDir inspects one candidate directory. It returns the
// summed file size and an empty reason when every entry is a known eval
// artifact (per-tenant memory store files and/or a skills subtree); a
// non-empty reason means the directory must be skipped.
func classifyEvalResidueDir(dir string) (int64, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, "unreadable: " + err.Error()
	}
	var bytes int64
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.Type().IsRegular() && evalMemoryFiles[name]:
			if info, err := entry.Info(); err == nil {
				bytes += info.Size()
			}
		case entry.IsDir() && name == "skills":
			b, reason := classifySkillsSubtree(filepath.Join(dir, name))
			if reason != "" {
				return 0, reason
			}
			bytes += b
		default:
			return 0, fmt.Sprintf("unexpected entry %q", name)
		}
	}
	return bytes, ""
}

// classifySkillsSubtree accepts an empty or skill-files-only tree: directories
// plus regular files with known skill asset extensions. Symlinks, special
// files, and unknown file types disqualify the whole candidate.
func classifySkillsSubtree(dir string) (int64, string) {
	var bytes int64
	var reason string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			reason = "unreadable: " + err.Error()
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() || !skillFileExts[strings.ToLower(filepath.Ext(d.Name()))] {
			reason = fmt.Sprintf("unexpected entry %q under skills/", path)
			return fs.SkipAll
		}
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	if reason != "" {
		return 0, reason
	}
	if walkErr != nil {
		return 0, "unreadable: " + walkErr.Error()
	}
	return bytes, ""
}
