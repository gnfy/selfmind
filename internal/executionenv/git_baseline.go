package executionenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitBaseline is evidence that a physical root was a clean, standalone Git
// checkout at admission. It is a candidate for a separate execution view, not
// permission to create one or proof that external effects are independent.
type GitBaseline struct {
	Root      string `json:"root"`
	CommonDir string `json:"common_dir"`
	Commit    string `json:"commit"`
}

var ErrGitViewUnavailable = errors.New("clean Git execution view unavailable")

// InspectCleanGitBaseline does not modify the checkout or fetch refs. A
// missing Git binary, changed worktree, nested root, submodule, or timeout all
// fail closed; callers can keep the ordinary physical-root serialization.
func InspectCleanGitBaseline(ctx context.Context, root string) (GitBaseline, error) {
	if strings.TrimSpace(root) == "" {
		return GitBaseline{}, fmt.Errorf("%w: invalid root", ErrGitViewUnavailable)
	}
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || root == "." {
		return GitBaseline{}, fmt.Errorf("%w: invalid root", ErrGitViewUnavailable)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return GitBaseline{}, fmt.Errorf("%w: resolve root: %v", ErrGitViewUnavailable, err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return GitBaseline{}, fmt.Errorf("%w: root is not a directory", ErrGitViewUnavailable)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return GitBaseline{}, fmt.Errorf("%w: root has no Git metadata", ErrGitViewUnavailable)
	}
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(gitCtx, "git", append([]string{"-C", root, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		cmd.Env = cleanGitProbeEnv(os.Environ())
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%w: git %s: %v", ErrGitViewUnavailable, args[0], err)
		}
		return bytes.TrimSpace(output), nil
	}
	top, err := run("rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(string(top)) != root {
		return GitBaseline{}, fmt.Errorf("%w: root is not the repository top level", ErrGitViewUnavailable)
	}
	// A .git file denotes a linked worktree or submodule. Neither is a safe
	// source for a new managed view without topology and ownership evidence.
	meta, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !meta.IsDir() {
		return GitBaseline{}, fmt.Errorf("%w: linked worktree or submodule", ErrGitViewUnavailable)
	}
	configKeys, err := run("config", "--list", "--name-only")
	if err != nil || hasExecutableGitFilter(string(configKeys)) {
		return GitBaseline{}, fmt.Errorf("%w: executable Git filter configuration", ErrGitViewUnavailable)
	}
	status, err := run("status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return GitBaseline{}, fmt.Errorf("%w: checkout is dirty or cannot be inspected", ErrGitViewUnavailable)
	}
	// A clean superproject may still contain submodules at mutable commits.
	// Until each component has its own baseline, reject the aggregate view.
	submodules, err := run("ls-files", "--stage", "-z")
	if err != nil {
		return GitBaseline{}, err
	}
	for _, entry := range bytes.Split(submodules, []byte{0}) {
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return GitBaseline{}, fmt.Errorf("%w: submodule present", ErrGitViewUnavailable)
		}
	}
	head, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || len(head) != 40 && len(head) != 64 {
		return GitBaseline{}, fmt.Errorf("%w: no resolved commit", ErrGitViewUnavailable)
	}
	common, err := run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return GitBaseline{}, err
	}
	commonDir, err := filepath.EvalSymlinks(string(common))
	if err != nil {
		return GitBaseline{}, fmt.Errorf("%w: resolve common Git directory: %v", ErrGitViewUnavailable, err)
	}
	return GitBaseline{Root: root, CommonDir: commonDir, Commit: string(head)}, nil
}

func cleanGitProbeEnv(in []string) []string {
	in = BuildProcessEnv(in, DefaultProcessEnvPolicy())
	out := make([]string, 0, len(in)+3)
	for _, variable := range in {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		out = append(out, variable)
	}
	return append(out, "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

func hasExecutableGitFilter(configKeys string) bool {
	for _, key := range strings.Fields(configKeys) {
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "filter.") && (strings.HasSuffix(key, ".smudge") || strings.HasSuffix(key, ".clean") || strings.HasSuffix(key, ".process")) {
			return true
		}
	}
	return false
}
