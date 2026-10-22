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
	// Discovery answers only the two identity probes. Every later read is
	// pinned to the checkout's own .git or goes through a daemon-written
	// directory, as for all later source commands, so a commondir written
	// while freeze runs cannot redirect a read, and configuration written
	// after the screen cannot start a program.
	top, err := gitSourceProbe(gitCtx, root, "rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(top) != root {
		return GitBaseline{}, fmt.Errorf("%w: root is not the repository top level", ErrGitViewUnavailable)
	}
	// A .git file denotes a linked worktree or submodule. Neither is a safe
	// source for a new managed view without topology and ownership evidence.
	gitDir := filepath.Join(root, ".git")
	meta, err := os.Lstat(gitDir)
	if err != nil || !meta.IsDir() {
		return GitBaseline{}, fmt.Errorf("%w: linked worktree or submodule", ErrGitViewUnavailable)
	}
	// The baseline names the checkout's own .git, never a directory a
	// commondir file points to: Git resolves refs, and so would create a
	// delivered branch, wherever that file points. Each pinned read below
	// also refuses a commondir or gitdir file outright.
	common, err := gitSourceProbe(gitCtx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return GitBaseline{}, err
	}
	if resolved, err := filepath.EvalSymlinks(common); err != nil || resolved != gitDir {
		return GitBaseline{}, fmt.Errorf("%w: common Git directory is not the checkout's own .git", ErrGitViewUnavailable)
	}
	source := GitBaseline{Root: root, CommonDir: gitDir}
	format, err := readGitSourceFormat(gitCtx, source)
	if err != nil {
		return GitBaseline{}, err
	}
	if format.objectFormat != "sha1" {
		return GitBaseline{}, fmt.Errorf("%w: SHA-256 repositories are not supported for managed views yet", ErrGitViewUnavailable)
	}
	configKeys, err := gitSourceCommand(gitCtx, source, "config", "--list", "--name-only")
	if err != nil {
		return GitBaseline{}, err
	}
	if err := screenGitSourceConfigKeys(configKeys); err != nil {
		return GitBaseline{}, err
	}
	// A clean superproject may still contain submodules at mutable commits.
	// Until each component has its own baseline, reject the aggregate view.
	// Check before status: a status that descends into a submodule runs Git
	// with that submodule's own, unscreened configuration.
	entries, err := gitSourceCommand(gitCtx, source, "ls-files", "--stage", "-z")
	if err != nil {
		return GitBaseline{}, err
	}
	for _, entry := range strings.Split(entries, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return GitBaseline{}, fmt.Errorf("%w: submodule present", ErrGitViewUnavailable)
		}
	}
	if err := gitSourceObjectIndirection(gitDir); err != nil {
		return GitBaseline{}, err
	}
	head, err := resolveGitSourceRef(gitCtx, "", source, "HEAD")
	if id, ok := gitObjectIDField(head); err != nil || !ok || id != head {
		return GitBaseline{}, fmt.Errorf("%w: no resolved commit", ErrGitViewUnavailable)
	}
	// The screen above judged the configuration once; a Run that worked here
	// may still change it. Status hashes stat-dirty files through any filter
	// the effective configuration defines, so the checkout is read through
	// daemon-written configuration that carries only passive comparison keys.
	checkout, err := gitSourceCheckoutConfig(gitCtx, source)
	if err != nil {
		return GitBaseline{}, err
	}
	status, err := gitSourceCheckoutStatus(gitCtx, root, gitDir, head, checkout)
	if err != nil || len(status) != 0 {
		return GitBaseline{}, fmt.Errorf("%w: checkout is dirty or cannot be inspected", ErrGitViewUnavailable)
	}
	return GitBaseline{Root: root, CommonDir: gitDir, Commit: head}, nil
}

// gitSourceCheckoutKeys are the only source settings carried into the
// configuration that reads the source checkout at baseline freeze. Each
// changes how status compares files; none can name a program.
var gitSourceCheckoutKeys = []string{"core.filemode", "core.ignorecase", "core.precomposeunicode", "core.symlinks",
	"core.autocrlf", "core.eol", "core.checkstat", "core.trustctime", "core.safecrlf"}

// gitSourceCheckoutConfig captures the source's comparison settings, keeping
// only gitSourceCheckoutKeys with plain values, as daemon-written
// configuration for gitSourceCheckoutStatus.
func gitSourceCheckoutConfig(ctx context.Context, source GitBaseline) (string, error) {
	output, err := gitSourceCommand(ctx, source, "config", "--null", "--list")
	if err != nil {
		return "", err
	}
	values := make(map[string]string)
	for _, entry := range strings.Split(output, "\x00") {
		if key, value, ok := strings.Cut(entry, "\n"); ok {
			values[strings.ToLower(key)] = value
		}
	}
	var b strings.Builder
	b.WriteString("[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
	for _, key := range gitSourceCheckoutKeys {
		if value, ok := values[key]; ok && plainGitConfigValue(value) {
			fmt.Fprintf(&b, "\t%s = %s\n", strings.TrimPrefix(key, "core."), value)
		}
	}
	return b.String(), nil
}

func plainGitConfigValue(value string) bool {
	if value == "" || len(value) > 16 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

// gitSourceCheckoutStatus reads the source checkout's status through a
// daemon-written Git directory, so a filter, fsmonitor, or include written
// into the source configuration after the screen cannot run.
func gitSourceCheckoutStatus(ctx context.Context, root, commonDir, head, config string) (string, error) {
	reader, err := newGitWorktreeReader("", root, commonDir, filepath.Join(commonDir, "objects"), head, config)
	if err != nil {
		return "", err
	}
	defer reader.close()
	return reader.command(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=all")
}

func cleanGitProbeEnv(in []string) []string {
	in = BuildProcessEnv(in, DefaultProcessEnvPolicy())
	out := make([]string, 0, len(in)+5)
	for _, variable := range in {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		out = append(out, variable)
	}
	// System and global configuration are ignored, so the only configuration a
	// source repository supplies is its own `.git/config`, which a Run can
	// write. Transport is blocked and a promisor remote cannot trigger a lazy
	// fetch, so a URL a Run plants cannot make Git launch ssh, a proxy, or a
	// credential helper; combined with the key screen this closes the paths by
	// which source files could run a program.
	return append(out, "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1")
}

// screenGitSourceConfigKeys refuses source configuration a sandboxed Run can
// write that would run a program or redirect transport or object resolution
// during a daemon-side command. Keys like core.sshCommand, credential.helper,
// url.*.insteadOf, core.gitProxy, and protocol.* are deliberately not refused:
// they are ordinary in a developer's real checkout and are instead neutralized
// by never running a transport and by GIT_ALLOW_PROTOCOL, GIT_NO_LAZY_FETCH,
// and the pinned hooks path. Directory hooks are already redirected to /dev/null
// by every invocation; configuration-defined hooks (hook.*) cannot be cleared
// per command and so are refused here. The flattened listing expands includes,
// so a key pulled in through include/includeIf is screened the same way.
func screenGitSourceConfigKeys(configKeys string) error {
	for _, key := range strings.Fields(configKeys) {
		key = strings.ToLower(key)
		switch {
		case strings.HasPrefix(key, "filter.") && (strings.HasSuffix(key, ".smudge") || strings.HasSuffix(key, ".clean") || strings.HasSuffix(key, ".process")),
			strings.HasPrefix(key, "diff.") && strings.HasSuffix(key, ".textconv"),
			key == "diff.external":
			return fmt.Errorf("%w: source repository defines an executable Git content driver (%s)", ErrGitViewUnavailable, key)
		case strings.HasPrefix(key, "hook."):
			return fmt.Errorf("%w: source repository defines a configuration-based Git hook (%s)", ErrGitViewUnavailable, key)
		case strings.HasPrefix(key, "uploadpack.") || strings.HasPrefix(key, "receivepack.") || strings.HasPrefix(key, "receive."):
			return fmt.Errorf("%w: source repository defines a pack-protocol program (%s)", ErrGitViewUnavailable, key)
		case key == "core.alternaterefscommand" || key == "core.alternaterefsprefixes":
			return fmt.Errorf("%w: source repository redirects alternate reference enumeration (%s)", ErrGitViewUnavailable, key)
		case key == "core.worktree":
			return fmt.Errorf("%w: source repository redirects its work tree (%s)", ErrGitViewUnavailable, key)
		}
	}
	return nil
}

// gitSourceObjectIndirection reports source object stores the repository did not
// have at baseline. A Run that borrows objects through an added alternates file
// could make a delivered or protected commit resolve against attacker-controlled
// data; a clean standalone baseline has none.
func gitSourceObjectIndirection(commonDir string) error {
	for _, name := range []string{"alternates", "http-alternates"} {
		path := filepath.Join(commonDir, "objects", "info", name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%w: source repository borrows objects through %s", ErrGitViewUnavailable, name)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("%w: inspect source object binding: %v", ErrGitViewUnavailable, err)
		}
	}
	return nil
}
