package executionenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitView is a detached, managed checkout pinned to an admitted Git baseline.
// Its Git metadata is inside the view: a linked worktree would put its index,
// objects, and refs under the original .git directory and require write access
// to that shared directory from the isolated process sandbox.
type GitView struct {
	ID       string      `json:"id"`
	Path     string      `json:"path"`
	Baseline GitBaseline `json:"baseline"`
}

// EnsureGitView is idempotent for an exact view ID and baseline. A stale or
// partially created directory is left in place for inspection, never reused
// or deleted blindly. The caller must durably bind the returned view to its
// Run before exposing it as an execution root.
func EnsureGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitView, error) {
	if !safeGitViewID(viewID) || !filepath.IsAbs(viewsDir) || baseline.Root == "" || baseline.CommonDir == "" || baseline.Commit == "" {
		return GitView{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	viewsDir = filepath.Clean(viewsDir)
	physicalParent, err := filepath.EvalSymlinks(filepath.Dir(viewsDir))
	if err != nil {
		return GitView{}, fmt.Errorf("%w: resolve view parent: %v", ErrGitViewUnavailable, err)
	}
	proposedDir := filepath.Join(physicalParent, filepath.Base(viewsDir))
	if overlappingGitViewRoots(proposedDir, baseline.Root) || overlappingGitViewRoots(proposedDir, baseline.CommonDir) {
		return GitView{}, fmt.Errorf("%w: view storage overlaps the source repository", ErrGitViewUnavailable)
	}
	if err := os.MkdirAll(viewsDir, 0o700); err != nil {
		return GitView{}, fmt.Errorf("%w: create view directory: %v", ErrGitViewUnavailable, err)
	}
	physicalDir, err := filepath.EvalSymlinks(viewsDir)
	if err != nil {
		return GitView{}, fmt.Errorf("%w: resolve view directory: %v", ErrGitViewUnavailable, err)
	}
	if overlappingGitViewRoots(physicalDir, baseline.Root) || overlappingGitViewRoots(physicalDir, baseline.CommonDir) {
		return GitView{}, fmt.Errorf("%w: view storage overlaps the source repository", ErrGitViewUnavailable)
	}
	view := GitView{ID: viewID, Path: filepath.Join(physicalDir, viewID), Baseline: baseline}
	if _, err := os.Lstat(view.Path); err == nil {
		return inspectExistingGitView(ctx, view)
	} else if !os.IsNotExist(err) {
		return GitView{}, fmt.Errorf("%w: inspect view: %v", ErrGitViewUnavailable, err)
	}
	if _, err := gitViewCommand(ctx, baseline.Root, "cat-file", "-e", baseline.Commit+"^{commit}"); err != nil {
		return GitView{}, err
	}
	common, err := gitViewCommand(ctx, baseline.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Clean(common) != filepath.Clean(baseline.CommonDir) {
		return GitView{}, fmt.Errorf("%w: source repository identity changed", ErrGitViewUnavailable)
	}
	configKeys, err := gitViewCommand(ctx, baseline.Root, "config", "--list", "--name-only")
	if err != nil || hasExecutableGitFilter(configKeys) {
		return GitView{}, fmt.Errorf("%w: executable Git filter configuration", ErrGitViewUnavailable)
	}
	if err := os.Mkdir(view.Path, 0o700); err != nil {
		// A concurrent creator may have won the same ID. It is safe to return
		// only after verifying the complete path and exact baseline.
		if existing, inspectErr := inspectExistingGitView(ctx, view); inspectErr == nil {
			return existing, nil
		}
		return GitView{}, fmt.Errorf("%w: reserve view path: %v", ErrGitViewUnavailable, err)
	}
	// Git alternates share only immutable source objects. All mutable Git state
	// (index, newly written objects, HEAD, refs) remains inside the sandbox's
	// writable view. We deliberately retain a partial directory on failure.
	if _, err := gitViewCommand(ctx, view.Path, "init", "-q"); err != nil {
		return GitView{}, err
	}
	objects, err := filepath.EvalSymlinks(filepath.Join(baseline.CommonDir, "objects"))
	if err != nil {
		return GitView{}, fmt.Errorf("%w: source objects unavailable: %v", ErrGitViewUnavailable, err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, ".git", "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return GitView{}, fmt.Errorf("%w: bind source objects: %v", ErrGitViewUnavailable, err)
	}
	if _, err := gitViewCommand(ctx, view.Path, "config", "--local", "core.hooksPath", os.DevNull); err != nil {
		return GitView{}, err
	}
	for _, key := range []string{"user.name", "user.email"} {
		if value, err := gitViewCommand(ctx, baseline.Root, "config", "--local", "--get", key); err == nil && value != "" {
			if _, err := gitViewCommand(ctx, view.Path, "config", "--local", key, value); err != nil {
				return GitView{}, err
			}
		}
	}
	if _, err := gitViewCommand(ctx, view.Path, "checkout", "--detach", "-f", baseline.Commit); err != nil {
		return GitView{}, err
	}
	// The source may later advance and prune objects no longer reachable from
	// its own refs. Copy the view's reachable baseline graph into its private
	// object store now; keep the alternates record for identity checks and old
	// view compatibility. A failed copy leaves the partial view for inspection.
	if _, err := gitViewCommandRawTimeout(ctx, view.Path, 5*time.Minute, "repack", "-a"); err != nil {
		return GitView{}, err
	}
	return inspectExistingGitView(ctx, view)
}

// InspectGitView validates an already-bound view without recreating it. A
// missing view may have contained uncommitted work, so recovery must park it
// rather than silently rebuilding an empty checkout from the base commit.
func InspectGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitView, error) {
	if !safeGitViewID(viewID) || !filepath.IsAbs(viewsDir) || baseline.Root == "" || baseline.CommonDir == "" || baseline.Commit == "" {
		return GitView{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	physicalDir, err := filepath.EvalSymlinks(filepath.Clean(viewsDir))
	if err != nil {
		return GitView{}, fmt.Errorf("%w: resolve view directory: %v", ErrGitViewUnavailable, err)
	}
	if overlappingGitViewRoots(physicalDir, baseline.Root) || overlappingGitViewRoots(physicalDir, baseline.CommonDir) {
		return GitView{}, fmt.Errorf("%w: view storage overlaps the source repository", ErrGitViewUnavailable)
	}
	return inspectExistingGitView(ctx, GitView{ID: viewID, Path: filepath.Join(physicalDir, viewID), Baseline: baseline})
}

func overlappingGitViewRoots(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func inspectExistingGitView(ctx context.Context, view GitView) (GitView, error) {
	info, err := os.Lstat(view.Path)
	if err != nil || !info.IsDir() {
		return GitView{}, fmt.Errorf("%w: existing view is missing or is not a directory", ErrGitViewUnavailable)
	}
	meta, err := os.Lstat(filepath.Join(view.Path, ".git"))
	if err != nil || !meta.IsDir() {
		return GitView{}, fmt.Errorf("%w: existing view has no independent Git metadata", ErrGitViewUnavailable)
	}
	if err := validateGitViewConfig(ctx, view.Path); err != nil {
		return GitView{}, err
	}
	gitDir, err := gitViewCommand(ctx, view.Path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return GitView{}, err
	}
	if filepath.Clean(gitDir) != filepath.Join(view.Path, ".git") {
		return GitView{}, fmt.Errorf("%w: Git metadata escaped the view", ErrGitViewUnavailable)
	}
	alternatePath := filepath.Join(view.Path, ".git", "objects", "info", "alternates")
	alternateInfo, err := os.Lstat(alternatePath)
	if err != nil || !alternateInfo.Mode().IsRegular() {
		return GitView{}, fmt.Errorf("%w: source object binding is missing", ErrGitViewUnavailable)
	}
	alternate, err := os.ReadFile(alternatePath)
	if err != nil {
		return GitView{}, fmt.Errorf("%w: read source object binding: %v", ErrGitViewUnavailable, err)
	}
	objects, err := filepath.EvalSymlinks(filepath.Join(view.Baseline.CommonDir, "objects"))
	if err != nil || string(alternate) != objects+"\n" {
		return GitView{}, fmt.Errorf("%w: source object binding changed", ErrGitViewUnavailable)
	}
	head, err := gitViewCommand(ctx, view.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return GitView{}, err
	}
	if head != view.Baseline.Commit {
		if _, err := gitViewCommand(ctx, view.Path, "merge-base", "--is-ancestor", view.Baseline.Commit, head); err != nil {
			return GitView{}, fmt.Errorf("%w: existing view left its baseline", ErrGitViewUnavailable)
		}
	}
	return view, nil
}

// A model can write the managed checkout, including its .git/config. Every
// later daemon-side Git probe and /apply import must refuse command-bearing
// repository configuration before invoking Git in that checkout. The view is
// initialized with only these passive keys; unexpected additions preserve it
// for inspection instead of being interpreted by a host-side Git process.
func validateGitViewConfig(ctx context.Context, viewPath string) error {
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(gitCtx, "git", "config", "--file", filepath.Join(viewPath, ".git", "config"),
		"--null", "--list", "--no-includes")
	cmd.Env = cleanGitProbeEnv(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: view Git configuration is unreadable", ErrGitViewUnavailable)
	}
	seen := make(map[string]bool)
	for _, entry := range strings.Split(string(output), "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "\n")
		key = strings.ToLower(key)
		if !ok || seen[key] {
			return fmt.Errorf("%w: view Git configuration is ambiguous", ErrGitViewUnavailable)
		}
		seen[key] = true
		switch key {
		case "core.repositoryformatversion":
			if value != "0" {
				return fmt.Errorf("%w: view Git format changed", ErrGitViewUnavailable)
			}
		case "core.filemode", "core.ignorecase", "core.precomposeunicode", "core.symlinks":
			if value != "true" && value != "false" {
				return fmt.Errorf("%w: view Git configuration changed", ErrGitViewUnavailable)
			}
		case "core.bare":
			if value != "false" {
				return fmt.Errorf("%w: view became a bare repository", ErrGitViewUnavailable)
			}
		case "core.logallrefupdates":
			if value != "true" {
				return fmt.Errorf("%w: view Git reference policy changed", ErrGitViewUnavailable)
			}
		case "core.hookspath":
			if value != os.DevNull {
				return fmt.Errorf("%w: view Git hooks changed", ErrGitViewUnavailable)
			}
		case "user.name", "user.email":
			if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
				return fmt.Errorf("%w: view Git identity changed unexpectedly", ErrGitViewUnavailable)
			}
		default:
			return fmt.Errorf("%w: view Git configuration contains unsupported key %q", ErrGitViewUnavailable, key)
		}
	}
	if !seen["core.repositoryformatversion"] || !seen["core.bare"] || !seen["core.hookspath"] {
		return fmt.Errorf("%w: view Git configuration is incomplete", ErrGitViewUnavailable)
	}
	return nil
}

func safeGitViewID(id string) bool {
	if len(id) == 0 || len(id) > 100 {
		return false
	}
	for _, char := range id {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func gitViewCommand(ctx context.Context, root string, args ...string) (string, error) {
	output, err := gitViewCommandRaw(ctx, root, args...)
	return strings.TrimSpace(output), err
}

func gitViewCommandRaw(ctx context.Context, root string, args ...string) (string, error) {
	return gitViewCommandRawTimeout(ctx, root, 15*time.Second, args...)
}

func gitViewCommandRawTimeout(ctx context.Context, root string, timeout time.Duration, args ...string) (string, error) {
	gitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(gitCtx, "git", append([]string{"-C", root, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Env = cleanGitProbeEnv(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: git %s: %v", ErrGitViewUnavailable, args[0], err)
	}
	return string(output), nil
}
