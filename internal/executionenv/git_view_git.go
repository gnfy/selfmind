package executionenv

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Daemon-side Git runs on the host, outside every Run sandbox, while a managed
// view and its .git directory are writable by the Run. These overrides disable
// the programs Git could otherwise start on its own, and the derived indexes
// (commit-graphs, multi-pack indexes, reachability bitmaps) a writer of the
// view or the source could forge, so commands read commits and objects
// themselves. The derived indexes are off as defense in depth: no decision
// that could lose work relies on Git reading them. The work listing, the
// proof, and the index check hash every commit and tree they rely on; the copy
// check reads each pack and the multi-pack index explicitly; prune's
// reachability walk reads only the view's own object directory, which carries
// no commit-graph. Only a view's validity check (merge-base), whose wrong pass
// makes the verified walk list more, and an informational count (diff) would
// read them otherwise. Command-line settings win over any repository
// configuration.
var gitHostSafetyConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.untrackedCache=false",
	"-c", "core.commitGraph=false",
	"-c", "core.multiPackIndex=false",
	"-c", "pack.useBitmaps=false",
}

// hostGitCommand prepares one daemon-side Git command with the host safety
// overrides ahead of its arguments.
func hostGitCommand(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, gitHostSafetyConfig...), args...)...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd
}

// gitViewEnv pins the exact Git directory, common directory, and work tree of
// one view. Discovery, a gitfile, or a commondir file can never choose another
// repository or configuration. Once a Run may have written the view, commands
// run with it must not read the checkout through content filters: rev-parse,
// merge-base, for-each-ref, rev-list, cat-file, show-index, and tree-to-tree
// diff qualify. Only creation, before any Run is bound, checks files out here.
func gitViewEnv(viewPath string) []string {
	gitDir := filepath.Join(viewPath, ".git")
	return append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+gitDir, "GIT_COMMON_DIR="+gitDir, "GIT_WORK_TREE="+viewPath,
		"GIT_NO_REPLACE_OBJECTS=1")
}

func gitViewCommand(ctx context.Context, viewPath string, args ...string) (string, error) {
	output, err := gitViewCommandRaw(ctx, viewPath, args...)
	return strings.TrimSpace(output), err
}

func gitViewCommandRaw(ctx context.Context, viewPath string, args ...string) (string, error) {
	return gitViewCommandRawTimeout(ctx, viewPath, 15*time.Second, args...)
}

func gitViewCommandRawTimeout(ctx context.Context, viewPath string, timeout time.Duration, args ...string) (string, error) {
	return runHostGit(ctx, timeout, viewPath, gitViewEnv(viewPath), nil, args...)
}

// gitPipeEnd is one side of a host Git pipeline: its working directory,
// pinned environment, arguments, and (for the producer) stdin.
type gitPipeEnd struct {
	dir   string
	env   []string
	stdin io.Reader
	args  []string
}

// pipeHostGit streams the producer's stdout into the consumer's stdin through
// an OS pipe between the two processes, so data of any size, such as a pack
// on its way from a view to the source, passes without the daemon holding it.
// Each side keeps its own environment and reports its own failure with its
// own stderr; cancellation or the deadline stops both, and so does the
// consumer's failure.
func pipeHostGit(ctx context.Context, timeout time.Duration, producer, consumer gitPipeEnd) error {
	gitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	from := hostGitCommand(gitCtx, producer.dir, producer.env, producer.args...)
	to := hostGitCommand(gitCtx, consumer.dir, consumer.env, consumer.args...)
	var fromErr, toErr gitStderr
	from.Stdin, from.Stderr, to.Stderr = producer.stdin, &fromErr, &toErr
	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("%w: git %s: %w", ErrGitViewUnavailable, gitSubcommand(producer.args), err)
	}
	from.Stdout, to.Stdin = write, read
	if err := from.Start(); err != nil {
		read.Close()
		write.Close()
		return fmt.Errorf("%w: git %s: %w", ErrGitViewUnavailable, gitSubcommand(producer.args), err)
	}
	if err := to.Start(); err != nil {
		read.Close()
		write.Close()
		cancel()
		_ = from.Wait()
		return fmt.Errorf("%w: git %s: %w", ErrGitViewUnavailable, gitSubcommand(consumer.args), err)
	}
	// The children hold their own ends. Closing the daemon's copies lets the
	// consumer see the end of the stream and the producer a reader that left.
	read.Close()
	write.Close()
	toWait := to.Wait()
	ctxErr := gitCtx.Err()
	if toWait != nil {
		cancel()
	}
	fromWait := from.Wait()
	if ctxErr != nil {
		return fmt.Errorf("%w: git %s into git %s did not finish: %w", ErrGitViewUnavailable,
			gitSubcommand(producer.args), gitSubcommand(consumer.args), ctxErr)
	}
	var failures []error
	if fromWait != nil {
		failures = append(failures, fmt.Errorf("git %s: %w: %s", gitSubcommand(producer.args), fromWait, strings.TrimSpace(fromErr.String())))
	}
	if toWait != nil {
		failures = append(failures, fmt.Errorf("git %s: %w: %s", gitSubcommand(consumer.args), toWait, strings.TrimSpace(toErr.String())))
	}
	switch len(failures) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%w: %w", ErrGitViewUnavailable, failures[0])
	default:
		return fmt.Errorf("%w: %w; %w", ErrGitViewUnavailable, failures[0], failures[1])
	}
}

func runHostGit(ctx context.Context, timeout time.Duration, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	gitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := hostGitCommand(gitCtx, dir, env, args...)
	cmd.Stdin = stdin
	var stderr gitStderr
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", gitCommandError(gitCtx, args, err, &stderr)
	}
	return string(output), nil
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C", "-c":
			i++
		default:
			return args[i]
		}
	}
	return ""
}

// gitWorktreeReader is a daemon-owned Git directory used only to read one
// checkout and its index: a managed view, or the source checkout at baseline
// freeze. Status refreshes hash files through any filter the effective
// configuration defines, so the configuration here is written by the daemon
// from validated passive keys: editing .git/config, adding a commondir, or
// defining a filter or fsmonitor after the screen cannot make a host-side
// status start a program. HEAD is the inspected commit; the index and object
// store remain the checkout's own data.
type gitWorktreeReader struct {
	workTree string
	gitDir   string
	objects  string
	dir      string
}

// newGitWorktreeReader creates the reader under scratch, a directory no Run
// can write ("" means the daemon's temporary directory).
func newGitWorktreeReader(scratch, workTree, gitDir, objects, head, config string) (*gitWorktreeReader, error) {
	dir, err := os.MkdirTemp(scratch, ".inspect-")
	if err != nil {
		return nil, fmt.Errorf("%w: create inspection directory: %v", ErrGitViewUnavailable, err)
	}
	reader := &gitWorktreeReader{workTree: workTree, gitDir: gitDir, objects: objects, dir: dir}
	if err := reader.write(head, config); err != nil {
		reader.close()
		return nil, err
	}
	return reader, nil
}

// newGitViewWorktree reads a managed view through validated passive settings,
// with its scratch directory beside the view in daemon-owned storage.
func newGitViewWorktree(view GitView, state gitViewState) (*gitWorktreeReader, error) {
	gitDir := filepath.Join(view.Path, ".git")
	return newGitWorktreeReader(filepath.Dir(view.Path), view.Path, gitDir, filepath.Join(gitDir, "objects"),
		state.head, state.config.inspectionConfig())
}

func (r *gitWorktreeReader) write(head, config string) error {
	exclude, err := readGitExclude(r.gitDir)
	if err != nil {
		return err
	}
	for _, sub := range []string{"refs", "info"} {
		if err := os.Mkdir(filepath.Join(r.dir, sub), 0o700); err != nil {
			return fmt.Errorf("%w: prepare inspection directory: %v", ErrGitViewUnavailable, err)
		}
	}
	files := map[string][]byte{
		"HEAD":                           []byte(head + "\n"),
		"config":                         []byte(config),
		filepath.Join("info", "exclude"): exclude,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(r.dir, name), data, 0o600); err != nil {
			return fmt.Errorf("%w: prepare inspection directory: %v", ErrGitViewUnavailable, err)
		}
	}
	return nil
}

func (r *gitWorktreeReader) close() { _ = os.RemoveAll(r.dir) }

func (r *gitWorktreeReader) command(ctx context.Context, args ...string) (string, error) {
	env := append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+r.dir, "GIT_COMMON_DIR="+r.dir, "GIT_WORK_TREE="+r.workTree,
		"GIT_INDEX_FILE="+filepath.Join(r.gitDir, "index"),
		"GIT_OBJECT_DIRECTORY="+r.objects,
		"GIT_NO_REPLACE_OBJECTS=1")
	return runHostGit(ctx, 15*time.Second, r.workTree, env, nil, args...)
}

// indexTree returns the tree the index records, as a commit of it would store
// it, without reading a tree object: write-tree builds it from the index's own
// entries and cached tree ids and, with --missing-ok, looks nothing up in the
// object store. It runs on a copy of the index with an empty object directory,
// since write-tree may rewrite the index and writes the trees it builds.
func (r *gitWorktreeReader) indexTree(ctx context.Context) (string, error) {
	index := filepath.Join(r.dir, "index")
	if err := copyGitIndex(filepath.Join(r.gitDir, "index"), index); err != nil {
		return "", err
	}
	objects := filepath.Join(r.dir, "objects")
	if err := os.Mkdir(objects, 0o700); err != nil {
		return "", fmt.Errorf("%w: prepare inspection directory: %v", ErrGitViewUnavailable, err)
	}
	env := append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+r.dir, "GIT_COMMON_DIR="+r.dir, "GIT_INDEX_FILE="+index,
		"GIT_OBJECT_DIRECTORY="+objects, "GIT_NO_REPLACE_OBJECTS=1")
	tree, err := runHostGit(ctx, 15*time.Second, r.dir, env, nil, "write-tree", "--missing-ok")
	return strings.TrimSpace(tree), err
}

// copyGitIndex copies a checkout's index. A missing index is an empty one; a
// link or anything but a regular file is refused, so it cannot redirect or
// stall the copy.
func copyGitIndex(from, to string) error {
	info, err := os.Lstat(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: index is not a regular file", ErrGitViewUnavailable)
	}
	in, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("%w: read index: %v", ErrGitViewUnavailable, err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: copy index: %v", ErrGitViewUnavailable, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("%w: copy index: %v", ErrGitViewUnavailable, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("%w: copy index: %v", ErrGitViewUnavailable, err)
	}
	return nil
}

// Exclude patterns are data. Copy a bounded regular file so a link, FIFO, or
// oversized file cannot redirect or stall the inspection.
func readGitExclude(gitDir string) ([]byte, error) {
	path := filepath.Join(gitDir, "info", "exclude")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("%w: exclude file is not a small regular file", ErrGitViewUnavailable)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read exclude file: %v", ErrGitViewUnavailable, err)
	}
	return data, nil
}

// streamHostGit passes each output line to visit as Git produces it, so an
// object listing for a long history is never buffered whole.
func streamHostGit(ctx context.Context, timeout time.Duration, dir string, env []string, stdin io.Reader, visit func(string), args ...string) error {
	gitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := hostGitCommand(gitCtx, dir, env, args...)
	cmd.Stdin = stdin
	var stderr gitStderr
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: git %s: %w", ErrGitViewUnavailable, gitSubcommand(args), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: git %s: %w", ErrGitViewUnavailable, gitSubcommand(args), err)
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		visit(scanner.Text())
	}
	if scanErr := scanner.Err(); scanErr != nil {
		// Stop a writer blocked on a pipe nobody reads any more.
		cancel()
		_ = cmd.Wait()
		return fmt.Errorf("%w: git %s: read output: %w", ErrGitViewUnavailable, gitSubcommand(args), scanErr)
	}
	if err := cmd.Wait(); err != nil {
		return gitCommandError(gitCtx, args, err, &stderr)
	}
	return nil
}

// gitStderr keeps the start of a Git command's stderr for its error, bounded
// so a verbose command cannot grow daemon memory.
type gitStderr struct{ bytes.Buffer }

const gitStderrLimit = 4 << 10

func (s *gitStderr) Write(p []byte) (int, error) {
	if room := gitStderrLimit - s.Len(); room > 0 {
		s.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// gitCommandError keeps what a failed Git command means: a deadline or
// cancellation is reported as such, and any other failure carries the start
// of Git's own stderr.
func gitCommandError(gitCtx context.Context, args []string, err error, stderr *gitStderr) error {
	if ctxErr := gitCtx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: git %s did not finish: %w", ErrGitViewUnavailable, gitSubcommand(args), ctxErr)
	}
	return fmt.Errorf("%w: git %s: %w: %s", ErrGitViewUnavailable, gitSubcommand(args), err, strings.TrimSpace(stderr.String()))
}

// gitObjectIDField returns the leading object ID of one Git output line.
func gitObjectIDField(line string) (string, bool) {
	field, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	if len(field) != 40 && len(field) != 64 {
		return "", false
	}
	for _, char := range field {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return "", false
		}
	}
	return field, true
}
