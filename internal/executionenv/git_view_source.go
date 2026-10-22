package executionenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The person's original checkout was not created by SelfMind, and a Run that
// works in it directly can write its .git at any moment, including between a
// daemon-side screen and the command the screen guarded. Source commands are
// therefore of four kinds. Identity probes resolve the repository the way the
// person's own Git would, to detect a changed topology. Config, object, and
// index reads and the object import of delivery run pinned to the admitted Git
// directory and have no hook event, transport, or lazy fetch. Ref reads and ref
// creation go through a daemon-written Git directory (gitSourceRefDir): pinning
// GIT_DIR and GIT_COMMON_DIR keeps config and objects in place, but Git still
// follows a planted commondir file for refs. The checkout status of baseline
// freeze reads daemon-written configuration (gitSourceCheckoutStatus).

// gitSourceProbe resolves the source repository by ordinary discovery. Use it
// only for identity probes that must notice a changed topology.
func gitSourceProbe(ctx context.Context, root string, args ...string) (string, error) {
	output, err := runHostGit(ctx, 15*time.Second, root, cleanGitProbeEnv(os.Environ()), nil, append([]string{"-C", root}, args...)...)
	return strings.TrimSpace(output), err
}

// gitSourceCommand runs one config, object, or index read of the admitted
// source repository with its Git and common directories pinned. runHostGit
// pins core.hooksPath and disables fsmonitor, cleanGitProbeEnv blocks every
// transport and lazy fetch, and replace refs are ignored, as in a view. Refs
// are read through gitSourceRefDir; the only pinned ref read is the
// cross-check in gitSourceRefDir.verify.
func gitSourceCommand(ctx context.Context, baseline GitBaseline, args ...string) (string, error) {
	if err := checkGitSourceMetadata(baseline); err != nil {
		return "", err
	}
	env := append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+baseline.CommonDir, "GIT_COMMON_DIR="+baseline.CommonDir, "GIT_WORK_TREE="+baseline.Root,
		"GIT_NO_REPLACE_OBJECTS=1")
	output, err := runHostGit(ctx, 15*time.Second, baseline.Root, env, nil, args...)
	return strings.TrimSpace(output), err
}

// gitSourceRedirects are files through which Git would resolve the source
// repository's refs or worktree state somewhere else. An admitted baseline is
// a main repository whose common directory is its own .git, with none of them.
var gitSourceRedirects = []string{"commondir", "gitdir"}

func checkGitSourceMetadata(baseline GitBaseline) error {
	if filepath.Clean(baseline.CommonDir) != filepath.Join(baseline.Root, ".git") {
		return fmt.Errorf("%w: source Git directory is not the checkout's own .git", ErrGitViewUnavailable)
	}
	for _, name := range gitSourceRedirects {
		if _, err := os.Lstat(filepath.Join(baseline.CommonDir, name)); err == nil {
			return fmt.Errorf("%w: source repository Git metadata contains a %s indirection", ErrGitViewUnavailable, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect source Git metadata: %v", ErrGitViewUnavailable, err)
		}
	}
	return nil
}

// gitSourceFormat is the repository format the daemon-written ref directory
// repeats, so it reads and writes refs where the source's own Git does.
type gitSourceFormat struct {
	refStorage   string
	objectFormat string
}

// readGitSourceFormat reads the format the way Git does: from the repository's
// own config file, without includes. Git itself refuses a format value it does
// not know; one it knows but managed views do not support is refused here.
func readGitSourceFormat(ctx context.Context, baseline GitBaseline) (gitSourceFormat, error) {
	if err := checkGitSourceMetadata(baseline); err != nil {
		return gitSourceFormat{}, err
	}
	file := filepath.Join(baseline.CommonDir, "config")
	read := func(key, fallback string) (string, error) {
		value, err := gitSourceCommand(ctx, baseline, "config", "--file", file, "--default", fallback, "--get", key)
		if err != nil && ctx.Err() == nil {
			return "", fmt.Errorf("source repository format is unreadable: %w", err)
		}
		if err != nil {
			return "", err
		}
		return strings.ToLower(value), nil
	}
	refStorage, err := read("extensions.refstorage", "files")
	if err != nil {
		return gitSourceFormat{}, err
	}
	objectFormat, err := read("extensions.objectformat", "sha1")
	if err != nil {
		return gitSourceFormat{}, err
	}
	if refStorage != "files" && refStorage != "reftable" {
		return gitSourceFormat{}, fmt.Errorf("%w: source repository format is unsupported: %q ref storage", ErrGitViewUnavailable, refStorage)
	}
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return gitSourceFormat{}, fmt.Errorf("%w: source repository format is unsupported: %q objects", ErrGitViewUnavailable, objectFormat)
	}
	return gitSourceFormat{refStorage: refStorage, objectFormat: objectFormat}, nil
}

// config writes no reflog for the refs the daemon creates.
func (f gitSourceFormat) config() string {
	var b strings.Builder
	version := 0
	if f.refStorage != "files" || f.objectFormat != "sha1" {
		version = 1
	}
	fmt.Fprintf(&b, "[core]\n\trepositoryformatversion = %d\n\tbare = true\n\tlogallrefupdates = false\n", version)
	if version == 1 {
		b.WriteString("[extensions]\n")
		if f.refStorage != "files" {
			fmt.Fprintf(&b, "\trefStorage = %s\n", f.refStorage)
		}
		if f.objectFormat != "sha1" {
			fmt.Fprintf(&b, "\tobjectFormat = %s\n", f.objectFormat)
		}
	}
	return b.String()
}

// gitSourceHeadFile is the HEAD for the daemon-written ref directory. A files
// repository's HEAD names the current branch or commit and is copied after
// validation; reftable keeps HEAD in its tables, so Git's own stub is used.
func gitSourceHeadFile(commonDir string, format gitSourceFormat) (string, error) {
	if format.refStorage == "reftable" {
		return "ref: refs/heads/.invalid\n", nil
	}
	path := filepath.Join(commonDir, "HEAD")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("%w: read source HEAD: %v", ErrGitViewUnavailable, err)
	}
	var content string
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		// core.preferSymlinkRefs writes HEAD as a link to the branch it names.
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("%w: read source HEAD: %v", ErrGitViewUnavailable, err)
		}
		content = "ref: " + target
	case info.Mode().IsRegular() && info.Size() <= 512:
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%w: read source HEAD: %v", ErrGitViewUnavailable, err)
		}
		content = strings.TrimSpace(string(data))
	default:
		return "", fmt.Errorf("%w: source HEAD is not a plain file", ErrGitViewUnavailable)
	}
	if target, ok := strings.CutPrefix(content, "ref: "); ok {
		if !strings.HasPrefix(target, "refs/") || strings.Contains(target, "..") || strings.ContainsAny(target, " \t\r\n\x00\\") {
			return "", fmt.Errorf("%w: source HEAD names an unexpected ref", ErrGitViewUnavailable)
		}
		return "ref: " + target + "\n", nil
	}
	if id, ok := gitObjectIDField(content); ok && id == content {
		return content + "\n", nil
	}
	return "", fmt.Errorf("%w: source HEAD is neither a branch nor a commit", ErrGitViewUnavailable)
}

// gitSourceRefDir is a daemon-written Git directory for reading and creating
// the source repository's refs. Its refs, packed-refs, and reftable are the
// source's, linked in; its objects are the source's object directory; its
// configuration is the daemon's own and repeats only the source's repository
// format. It has no commondir of its own, so a commondir planted in the source
// cannot redirect its ref reads, and update-ref here runs no hook of any kind:
// Git runs configuration-defined hook.<name> reference-transaction hooks even
// with core.hooksPath=/dev/null, and none of the source's configuration is read.
type gitSourceRefDir struct {
	dir      string
	baseline GitBaseline
}

// newGitSourceRefDir prepares the directory under scratch, a directory no Run
// can write ("" means the daemon's temporary directory).
func newGitSourceRefDir(ctx context.Context, scratch string, baseline GitBaseline) (*gitSourceRefDir, error) {
	format, err := readGitSourceFormat(ctx, baseline)
	if err != nil {
		return nil, err
	}
	head, err := gitSourceHeadFile(baseline.CommonDir, format)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(scratch, ".refs-")
	if err != nil {
		return nil, fmt.Errorf("%w: create ref directory: %v", ErrGitViewUnavailable, err)
	}
	refs := &gitSourceRefDir{dir: dir, baseline: baseline}
	for name, content := range map[string]string{"HEAD": head, "config": format.config()} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			refs.close()
			return nil, fmt.Errorf("%w: prepare ref directory: %v", ErrGitViewUnavailable, err)
		}
	}
	for _, name := range []string{"refs", "packed-refs", "reftable"} {
		if err := os.Symlink(filepath.Join(baseline.CommonDir, name), filepath.Join(dir, name)); err != nil {
			refs.close()
			return nil, fmt.Errorf("%w: prepare ref directory: %v", ErrGitViewUnavailable, err)
		}
	}
	return refs, nil
}

func (r *gitSourceRefDir) close() { _ = os.RemoveAll(r.dir) }

func (r *gitSourceRefDir) env() []string {
	return append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+r.dir, "GIT_COMMON_DIR="+r.dir,
		"GIT_OBJECT_DIRECTORY="+filepath.Join(r.baseline.CommonDir, "objects"),
		"GIT_NO_REPLACE_OBJECTS=1")
}

func (r *gitSourceRefDir) command(ctx context.Context, args ...string) (string, error) {
	output, err := runHostGit(ctx, 15*time.Second, r.dir, r.env(), nil, args...)
	return strings.TrimSpace(output), err
}

// resolve returns the commit a source ref names.
func (r *gitSourceRefDir) resolve(ctx context.Context, ref string) (string, error) {
	return r.command(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

// errGitSourceRefOther marks a source ref that is absent or names other work,
// as opposed to a check of it that could not run.
var errGitSourceRefOther = errors.New("source ref does not name the work")

// gitRefAbsent reports rev-parse --verify --quiet failing because the name
// does not resolve to a commit, which it signals with exit status 1.
func gitRefAbsent(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

// gitInterrupted reports a Git command stopped by cancellation or a deadline.
func gitInterrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// verify proves ref names commit twice: through this directory, which a
// planted commondir cannot redirect, and through the source repository with
// its own configuration, as the person's own Git reads it. A ref written where
// the source does not look, such as a loose file in a reftable repository,
// fails the second read. An absent ref, or one naming other work, is reported
// as errGitSourceRefOther; a check that could not run keeps its own cause.
func (r *gitSourceRefDir) verify(ctx context.Context, ref, commit string) error {
	got, err := r.resolve(ctx, ref)
	if err != nil && !gitRefAbsent(err) {
		return fmt.Errorf("check source ref %s: %w", ref, err)
	}
	if err != nil || got != commit {
		return fmt.Errorf("%w: %w: %s does not name %s", ErrGitViewUnavailable, errGitSourceRefOther, ref, commit)
	}
	got, err = gitSourceCommand(ctx, r.baseline, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil && !gitRefAbsent(err) {
		return fmt.Errorf("check source ref %s with the source repository's own Git: %w", ref, err)
	}
	if err != nil || got != commit {
		return fmt.Errorf("%w: %w: the source repository does not resolve %s to %s", ErrGitViewUnavailable, errGitSourceRefOther, ref, commit)
	}
	return nil
}

// createGitSourceRef creates one ref in the admitted source repository and
// proves the repository resolves it before reporting success. Creation is
// compare-and-swap against an absent ref; an existing ref that already names
// the same commit is the result of an identical earlier creation.
func createGitSourceRef(ctx context.Context, scratch string, baseline GitBaseline, ref, commit string) error {
	refs, err := newGitSourceRefDir(ctx, scratch, baseline)
	if err != nil {
		return err
	}
	defer refs.close()
	if _, err := refs.command(ctx, "update-ref", ref, commit, strings.Repeat("0", len(commit))); err != nil {
		if gitInterrupted(err) {
			return err
		}
		existing, resolveErr := refs.resolve(ctx, ref)
		switch {
		case resolveErr != nil && !gitRefAbsent(resolveErr):
			return fmt.Errorf("check source ref %s: %w", ref, resolveErr)
		case resolveErr != nil:
			return fmt.Errorf("%w: source ref %s could not be created: %w", ErrGitViewUnavailable, ref, err)
		case existing != commit:
			return fmt.Errorf("%w: source ref %s already names other work", ErrGitViewUnavailable, ref)
		}
	}
	return refs.verify(ctx, ref, commit)
}

// verifyGitSourceRef checks one existing source ref the same way.
func verifyGitSourceRef(ctx context.Context, scratch string, baseline GitBaseline, ref, commit string) error {
	refs, err := newGitSourceRefDir(ctx, scratch, baseline)
	if err != nil {
		return err
	}
	defer refs.close()
	return refs.verify(ctx, ref, commit)
}

// resolveGitSourceRef reads one source ref through the daemon-written
// directory; a missing ref is an error the caller may treat as absent.
func resolveGitSourceRef(ctx context.Context, scratch string, baseline GitBaseline, ref string) (string, error) {
	refs, err := newGitSourceRefDir(ctx, scratch, baseline)
	if err != nil {
		return "", err
	}
	defer refs.close()
	return refs.resolve(ctx, ref)
}

// gitSourceObjectEnv pins the Git and object directories of the source
// repository for a plumbing command that writes objects, so no discovery,
// work tree, or index of the checkout is involved.
func gitSourceObjectEnv(baseline GitBaseline) []string {
	return append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+baseline.CommonDir, "GIT_COMMON_DIR="+baseline.CommonDir, "GIT_NO_REPLACE_OBJECTS=1")
}
