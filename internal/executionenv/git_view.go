package executionenv

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	if _, err := gitSourceCommand(ctx, baseline, "cat-file", "-e", baseline.Commit+"^{commit}"); err != nil {
		return GitView{}, err
	}
	common, err := gitSourceProbe(ctx, baseline.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Clean(common) != filepath.Clean(baseline.CommonDir) {
		return GitView{}, fmt.Errorf("%w: source repository identity changed", ErrGitViewUnavailable)
	}
	configKeys, err := gitSourceCommand(ctx, baseline, "config", "--list", "--name-only")
	if err != nil {
		return GitView{}, err
	}
	if err := screenGitSourceConfigKeys(configKeys); err != nil {
		return GitView{}, err
	}
	if err := gitSourceObjectIndirection(baseline.CommonDir); err != nil {
		return GitView{}, err
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
		if value, err := gitSourceCommand(ctx, baseline, "config", "--local", "--get", key); err == nil && value != "" {
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
	view, _, err := inspectBoundGitView(ctx, viewsDir, viewID, baseline)
	return view, err
}

func inspectBoundGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitView, gitViewState, error) {
	if !safeGitViewID(viewID) || !filepath.IsAbs(viewsDir) || baseline.Root == "" || baseline.CommonDir == "" || baseline.Commit == "" {
		return GitView{}, gitViewState{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	physicalDir, err := filepath.EvalSymlinks(filepath.Clean(viewsDir))
	if err != nil {
		return GitView{}, gitViewState{}, fmt.Errorf("%w: resolve view directory: %v", ErrGitViewUnavailable, err)
	}
	if overlappingGitViewRoots(physicalDir, baseline.Root) || overlappingGitViewRoots(physicalDir, baseline.CommonDir) {
		return GitView{}, gitViewState{}, fmt.Errorf("%w: view storage overlaps the source repository", ErrGitViewUnavailable)
	}
	view := GitView{ID: viewID, Path: filepath.Join(physicalDir, viewID), Baseline: baseline}
	if _, err := os.Lstat(view.Path); errors.Is(err, os.ErrNotExist) {
		return GitView{}, gitViewState{}, missingGitViewError(physicalDir, viewID)
	}
	state, err := inspectGitViewState(ctx, view)
	if err != nil {
		return GitView{}, gitViewState{}, err
	}
	return view, state, nil
}

// A Run that resumes after its view left the live path must learn where the
// work went instead of seeing an unexplained missing directory.
func missingGitViewError(viewsDir, viewID string) error {
	archive := filepath.Join(viewsDir, ".retired")
	if info, err := os.Lstat(filepath.Join(archive, ".pruned", viewID+".json")); err == nil && info.Mode().IsRegular() {
		return fmt.Errorf("%w: execution view was pruned after delivery; its committed work is retained at %s in the source repository and the checkout cannot be restored",
			ErrGitViewUnavailable, retainedGitViewRef(viewID))
	}
	if info, err := os.Lstat(filepath.Join(archive, viewID)); err == nil && info.IsDir() {
		return fmt.Errorf("%w: execution view was archived; restore it with /views restore <run_id> before continuing", ErrGitViewUnavailable)
	}
	if info, err := os.Lstat(filepath.Join(archive, ".pruning", viewID)); err == nil && info.IsDir() {
		return fmt.Errorf("%w: execution view is being pruned", ErrGitViewUnavailable)
	}
	return fmt.Errorf("%w: existing view is missing or is not a directory", ErrGitViewUnavailable)
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

// gitViewState is the validated evidence one inspection derived from a view:
// its passive repository settings and the exact HEAD commit it inspected.
type gitViewState struct {
	config gitViewConfig
	head   string
}

func inspectExistingGitView(ctx context.Context, view GitView) (GitView, error) {
	if _, err := inspectGitViewState(ctx, view); err != nil {
		return GitView{}, err
	}
	return view, nil
}

func inspectGitViewState(ctx context.Context, view GitView) (gitViewState, error) {
	info, err := os.Lstat(view.Path)
	if err != nil || !info.IsDir() {
		return gitViewState{}, fmt.Errorf("%w: existing view is missing or is not a directory", ErrGitViewUnavailable)
	}
	if err := validateGitViewMetadata(view); err != nil {
		return gitViewState{}, err
	}
	config, err := validateGitViewConfig(ctx, view.Path)
	if err != nil {
		return gitViewState{}, err
	}
	head, err := gitViewCommand(ctx, view.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return gitViewState{}, err
	}
	if head != view.Baseline.Commit {
		if _, err := gitViewCommand(ctx, view.Path, "merge-base", "--is-ancestor", view.Baseline.Commit, head); err != nil {
			return gitViewState{}, fmt.Errorf("%w: existing view left its baseline", ErrGitViewUnavailable)
		}
	}
	return gitViewState{config: config, head: head}, nil
}

// gitViewRedirects are files through which Git would read repository state
// from outside the view's own .git directory, or rewrite the history that
// reachability proofs rely on. A view is created without any of them.
var gitViewRedirects = []string{"commondir", "gitdir", "config.worktree", filepath.Join("info", "grafts"), "shallow"}

// validateGitViewMetadata checks the view's .git structure before any Git
// process reads it. Pinning GIT_DIR and GIT_COMMON_DIR already stops these
// files from steering daemon-side commands; rejecting them also keeps a
// redirected view from ever being delivered, archived, or resumed.
func validateGitViewMetadata(view GitView) error {
	gitDir := filepath.Join(view.Path, ".git")
	meta, err := os.Lstat(gitDir)
	if err != nil || !meta.IsDir() {
		return fmt.Errorf("%w: existing view has no independent Git metadata", ErrGitViewUnavailable)
	}
	for _, name := range gitViewRedirects {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return fmt.Errorf("%w: view Git metadata contains unsupported indirection %s", ErrGitViewUnavailable, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect view Git metadata: %v", ErrGitViewUnavailable, err)
		}
	}
	alternatePath := filepath.Join(gitDir, "objects", "info", "alternates")
	alternateInfo, err := os.Lstat(alternatePath)
	if err != nil || !alternateInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: source object binding is missing", ErrGitViewUnavailable)
	}
	alternate, err := os.ReadFile(alternatePath)
	if err != nil {
		return fmt.Errorf("%w: read source object binding: %v", ErrGitViewUnavailable, err)
	}
	objects, err := filepath.EvalSymlinks(filepath.Join(view.Baseline.CommonDir, "objects"))
	if err != nil || string(alternate) != objects+"\n" {
		return fmt.Errorf("%w: source object binding changed", ErrGitViewUnavailable)
	}
	return nil
}

// gitViewConfig is the complete set of passive settings a view may carry.
// The view is initialized with only these keys; anything else, including
// includes and every key that names a program, preserves the view for
// inspection instead of being interpreted by a host-side Git process.
type gitViewConfig struct {
	values map[string]string
}

var gitViewBooleanKeys = []string{"core.filemode", "core.ignorecase", "core.precomposeunicode", "core.symlinks"}

// inspectionConfig is what host-side worktree reads use instead of the
// view's writable .git/config.
func (c gitViewConfig) inspectionConfig() string {
	var b strings.Builder
	b.WriteString("[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
	for _, key := range gitViewBooleanKeys {
		if value, ok := c.values[key]; ok {
			fmt.Fprintf(&b, "\t%s = %s\n", strings.TrimPrefix(key, "core."), value)
		}
	}
	return b.String()
}

// validateGitViewConfig parses the view's own config file with Git itself, so
// the allowlist judges exactly the entries Git would read; metadata validation
// has already ruled out every other repository configuration source.
func validateGitViewConfig(ctx context.Context, viewPath string) (gitViewConfig, error) {
	output, err := runHostGit(ctx, 5*time.Second, viewPath, gitViewEnv(viewPath), nil,
		"config", "--file", filepath.Join(viewPath, ".git", "config"), "--null", "--list", "--no-includes")
	if err != nil {
		return gitViewConfig{}, fmt.Errorf("%w: view Git configuration is unreadable", ErrGitViewUnavailable)
	}
	config := gitViewConfig{values: make(map[string]string)}
	for _, entry := range strings.Split(output, "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "\n")
		key = strings.ToLower(key)
		if _, duplicate := config.values[key]; !ok || duplicate {
			return gitViewConfig{}, fmt.Errorf("%w: view Git configuration is ambiguous", ErrGitViewUnavailable)
		}
		config.values[key] = value
		if err := validateGitViewConfigEntry(key, value); err != nil {
			return gitViewConfig{}, err
		}
	}
	for _, required := range []string{"core.repositoryformatversion", "core.bare", "core.hookspath"} {
		if _, ok := config.values[required]; !ok {
			return gitViewConfig{}, fmt.Errorf("%w: view Git configuration is incomplete", ErrGitViewUnavailable)
		}
	}
	return config, nil
}

func validateGitViewConfigEntry(key, value string) error {
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
