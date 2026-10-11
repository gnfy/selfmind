package executionenv

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitViewDelivery describes the immutable commit contribution and any work
// that still exists only in the managed checkout. The view itself is never
// removed or rewritten by inspection or delivery.
type GitViewDelivery struct {
	ViewID         string
	Path           string
	SourceRoot     string
	BaselineCommit string
	HeadCommit     string
	CommittedFiles int
	// WorktreeInspected is false when only committed work was read because a
	// Run may still write the checkout; the counts below are then unknown.
	WorktreeInspected bool
	Uncommitted       int
	Untracked         int
	Ignored           int
	// HiddenIndexEntries counts entries git status cannot report: files
	// marked skip-worktree or assume-unchanged, and unmerged entries.
	HiddenIndexEntries int
	// LinkedWorktrees counts worktrees registered under .git/worktrees. Their
	// checkouts, HEADs, and indexes are outside the view's own status.
	LinkedWorktrees int
	// Submodules counts gitlink entries and submodule repositories under
	// .git/modules; status does not descend into them.
	Submodules      int
	DeliveredBranch string
}

// InspectGitViewDelivery reads committed work and the checkout. Worktree reads
// use daemon-written configuration, but a caller should still prefer
// InspectGitViewCommits while a Run may write the view: the counts would be a
// moving target and the checkout is the Run's to change.
func InspectGitViewDelivery(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitViewDelivery, error) {
	return inspectGitViewDelivery(ctx, viewsDir, viewID, baseline, true)
}

// InspectGitViewCommits reports only the committed contribution. It reads no
// checkout file and issues no command that can run a content filter.
func InspectGitViewCommits(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitViewDelivery, error) {
	return inspectGitViewDelivery(ctx, viewsDir, viewID, baseline, false)
}

func inspectGitViewDelivery(ctx context.Context, viewsDir, viewID string, baseline GitBaseline, worktree bool) (GitViewDelivery, error) {
	view, state, err := inspectBoundGitView(ctx, viewsDir, viewID, baseline)
	if err != nil {
		return GitViewDelivery{}, err
	}
	changed, err := gitViewCommandRaw(ctx, view.Path, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", baseline.Commit, state.head)
	if err != nil {
		return GitViewDelivery{}, err
	}
	result := GitViewDelivery{ViewID: viewID, Path: view.Path, SourceRoot: baseline.Root,
		BaselineCommit: baseline.Commit, HeadCommit: state.head, CommittedFiles: strings.Count(changed, "\x00")}
	if worktree {
		if err := inspectGitViewWorktree(ctx, view, state, &result); err != nil {
			return GitViewDelivery{}, err
		}
	}
	// A missing delivery ref is the ordinary "not delivered" answer, so the
	// lookup's own failure is not an error; a tampered or unsupported source
	// repository is, and is reported before it could masquerade as one. A
	// branch counts only while it names HEAD and the source holds every
	// object of the work with matching content: a ref to a commit object
	// copied there, or to placeholders at its trees and blobs, is not work the
	// source can rebuild.
	refs, err := newGitSourceRefDir(ctx, filepath.Dir(view.Path), baseline)
	if err != nil {
		return GitViewDelivery{}, err
	}
	defer refs.close()
	branch := "refs/heads/selfmind/" + viewID
	// verify also reads the branch with the source's own Git, which refuses a
	// repository format it no longer accepts. A check that could not run is
	// reported, not read as "not delivered".
	switch err := refs.verify(ctx, branch, state.head); {
	case err == nil:
		if err := proveGitWorkInSource(ctx, refs, view.Path, state.head, baseline.Commit); err == nil {
			result.DeliveredBranch = strings.TrimPrefix(branch, "refs/heads/")
		} else if !gitSourceLacksWork(err) {
			return GitViewDelivery{}, err
		}
	case !errors.Is(err, errGitSourceRefOther):
		return GitViewDelivery{}, err
	}
	return result, nil
}

func inspectGitViewWorktree(ctx context.Context, view GitView, state gitViewState, result *GitViewDelivery) error {
	worktree, err := newGitViewWorktree(view, state)
	if err != nil {
		return err
	}
	defer worktree.close()
	status, err := worktree.command(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=all")
	if err != nil {
		return err
	}
	fields := strings.Split(status, "\x00")
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if len(field) < 3 {
			continue
		}
		if strings.HasPrefix(field, "?? ") {
			result.Untracked++
		} else {
			result.Uncommitted++
		}
		// In -z output, rename/copy records carry a second NUL-delimited
		// path. It is metadata for this one change, not another file.
		if strings.ContainsAny(field[:2], "RC") {
			index++
		}
	}
	ignored, err := worktree.command(ctx, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	result.Ignored = strings.Count(ignored, "\x00")
	// -v tags every index entry: "H" is ordinary, "S" skip-worktree, "M"
	// unmerged, and a lowercase tag marks assume-unchanged.
	entries, err := worktree.command(ctx, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return err
	}
	var links []gitIndexLink
	for _, entry := range strings.Split(entries, "\x00") {
		if entry == "" {
			continue
		}
		if !strings.HasPrefix(entry, "H ") {
			result.HiddenIndexEntries++
		}
		if _, stage, ok := strings.Cut(entry, " "); ok && strings.HasPrefix(stage, "160000 ") {
			result.Submodules++
		}
		if link, ok := parseGitIndexLink(entry); ok {
			links = append(links, link)
		}
	}
	gitDir := filepath.Join(view.Path, ".git")
	if result.LinkedWorktrees, err = countGitViewEntries(filepath.Join(gitDir, "worktrees")); err != nil {
		return err
	}
	modules, err := countGitViewEntries(filepath.Join(gitDir, "modules"))
	if err != nil {
		return err
	}
	result.Submodules += modules
	// Status reads objects through Git without hashing them, and an object the
	// view does not store itself (a gc leaves out what the source holds) comes
	// from the source, where a planted one could hide a change. A checkout that
	// status calls clean is therefore checked again against objects the daemon
	// hashes. Hidden entries and submodules already stop archive and prune, and
	// status does not compare submodules, so there the index may differ from
	// HEAD.
	if result.Uncommitted == 0 && result.HiddenIndexEntries == 0 && result.Submodules == 0 {
		missed, err := gitChangesStatusMissed(ctx, worktree, view.Path, state.head, links)
		if err != nil {
			return err
		}
		result.Uncommitted += missed
	}
	result.WorktreeInspected = true
	return nil
}

// gitIndexLink is an ordinary symlink entry of the index.
type gitIndexLink struct{ path, id string }

// parseGitIndexLink reads a stage-0 symlink entry of ls-files --stage -v -z,
// "H 120000 <id> 0\t<path>".
func parseGitIndexLink(entry string) (gitIndexLink, bool) {
	info, path, ok := strings.Cut(entry, "\t")
	fields := strings.Fields(info)
	if !ok || len(fields) != 4 || fields[0] != "H" || fields[1] != "120000" || fields[3] != "0" {
		return gitIndexLink{}, false
	}
	return gitIndexLink{path: path, id: fields[2]}, true
}

// gitChangesStatusMissed checks a checkout that status calls clean against
// what the daemon hashes itself, and returns the changes status missed. The
// index's tree, built from its entries without reading a tree object, must be
// the tree of HEAD's verified commit, or the inspection fails. Git settles a
// symlink whose size still matches, or whose index entry carries no stat data
// (after read-tree, or a write in the same second as the link), by reading
// its blob unhashed; here every symlink's target is read from the checkout and
// hashed as a blob, and each one whose id is not its index entry's is a
// change, with no blob read from either store. A regular file is settled by
// hashing the file itself, so no blob decides it.
func gitChangesStatusMissed(ctx context.Context, worktree *gitWorktreeReader, viewPath, head string, links []gitIndexLink) (int, error) {
	recorded, err := worktree.indexTree(ctx)
	if err != nil {
		return 0, err
	}
	reader, err := newGitObjectReader(ctx, viewPath, gitViewEnv(viewPath), errGitViewIncomplete, errGitViewCorrupt)
	if err != nil {
		return 0, err
	}
	defer reader.close()
	kind, content, err := reader.read(head)
	if err != nil {
		return 0, err
	}
	if kind != "commit" {
		return 0, fmt.Errorf("%w: HEAD %s is a %s", ErrGitViewUnavailable, head, kind)
	}
	commit, err := parseGitCommit(head, content)
	if err != nil {
		return 0, err
	}
	if commit.tree != recorded {
		return 0, fmt.Errorf("%w: git status shows no change, but the index records tree %s and HEAD %s has tree %s, so a tree status compared is not HEAD's own",
			ErrGitViewUnavailable, recorded, head, commit.tree)
	}
	missed := 0
	for _, link := range links {
		path := filepath.Join(viewPath, filepath.FromSlash(link.path))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			// Git reports a missing entry from its lstat alone.
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("%w: inspect symlink %s: %v", ErrGitViewUnavailable, link.path, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// Another file type is a type change, or with core.symlinks off a
			// file Git hashes; neither is settled by the blob.
			continue
		}
		target, err := os.Readlink(path)
		if err != nil {
			return 0, fmt.Errorf("%w: read symlink %s: %v", ErrGitViewUnavailable, link.path, err)
		}
		if gitBlobID(link.id, []byte(target)) != link.id {
			missed++
		}
	}
	return missed, nil
}

// gitBlobID is the id content has as a blob, in the hash id is written in.
func gitBlobID(id string, content []byte) string {
	digest := gitObjectHash(id)
	fmt.Fprintf(digest, "blob %d\x00", len(content))
	digest.Write(content)
	return hex.EncodeToString(digest.Sum(nil))
}

func countGitViewEntries(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("%w: inspect %s: %v", ErrGitViewUnavailable, filepath.Base(dir), err)
	}
	return len(entries), nil
}

// DeliverGitViewBranch imports committed view work into a new, namespaced
// branch in the original repository. It never touches the checked-out branch,
// index, or worktree. A branch creation is atomic through update-ref's absent
// old-value check; a repeated request for the same commit is idempotent and
// takes the same path, import included. Success is reported only once the
// source holds the work with matching content and its own Git resolves the
// branch to the delivered commit. Source repository identity or view drift
// fails before the branch is created.
// Uncommitted and untracked files remain in the view for separate review instead of being
// silently dropped from an apparently complete delivery.
func DeliverGitViewBranch(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitViewDelivery, error) {
	result, err := InspectGitViewDelivery(ctx, viewsDir, viewID, baseline)
	if err != nil {
		return GitViewDelivery{}, err
	}
	// Decided below, by this delivery's own proof.
	result.DeliveredBranch = ""
	if result.Uncommitted != 0 || result.Untracked != 0 {
		return result, fmt.Errorf("%w: view has uncommitted or untracked work", ErrGitViewUnavailable)
	}
	if result.HeadCommit == baseline.Commit {
		return result, fmt.Errorf("%w: view has no committed changes to deliver", ErrGitViewUnavailable)
	}
	if err := inspectGitDeliverySource(ctx, baseline); err != nil {
		return result, err
	}
	branch := "refs/heads/selfmind/" + viewID
	// Imported with object plumbing, never a transport. A fetch would resolve
	// the view path and refs through the source repository's own config, which
	// a Run can write: url.*.insteadOf, core.sshCommand, core.gitProxy,
	// credential.*, protocol.*, a promisor remote, and core.alternateRefsCommand
	// all reach a program from a local fetch. Instead a pack is built in the
	// pinned, allowlisted view context and unpacked into the source object
	// store with GIT_DIR pinned, so Git never reads the source checkout's
	// config to move bytes. This may add unreachable objects on failure, but it
	// never changes the source checkout or a named ref before the final CAS.
	if err := ensureGitWorkInSource(ctx, baseline, result); err != nil {
		return result, err
	}
	if err := inspectGitDeliverySource(ctx, baseline); err != nil {
		return result, err
	}
	viewNow, err := InspectGitViewDelivery(ctx, viewsDir, viewID, baseline)
	if err != nil || viewNow.HeadCommit != result.HeadCommit || viewNow.Uncommitted != 0 || viewNow.Untracked != 0 {
		return result, fmt.Errorf("%w: view changed during delivery", ErrGitViewUnavailable)
	}
	// A concurrent identical delivery is success; a different branch tip is a
	// conflict and is never overwritten. Success is reported only after the
	// source repository itself resolves the branch to the delivered commit.
	if err := createGitSourceRef(ctx, filepath.Dir(result.Path), baseline, branch, result.HeadCommit); err != nil {
		return result, fmt.Errorf("delivery branch %s was not delivered: %w", strings.TrimPrefix(branch, "refs/heads/"), err)
	}
	result.DeliveredBranch = filepath.ToSlash(strings.TrimPrefix(branch, "refs/heads/"))
	return result, nil
}

// ensureGitWorkInSource imports the work from the view or its archive and then
// proves the source holds every object the work adds over the baseline, with
// matching content. Work counts as delivered or protected only after this.
// The import runs every time, even when the source appears to hold the work,
// and refuses a placeholder or substitute planted under one of the work's ids
// (see importGitViewCommit) instead of letting it pass for the work. A repeated
// import leaves a duplicate pack until Git repacks the source.
func ensureGitWorkInSource(ctx context.Context, baseline GitBaseline, work GitViewDelivery) error {
	if work.HeadCommit == baseline.Commit {
		return nil
	}
	if err := importGitViewCommit(ctx, baseline, work); err != nil {
		return err
	}
	refs, err := newGitSourceRefDir(ctx, filepath.Dir(work.Path), baseline)
	if err != nil {
		return err
	}
	defer refs.close()
	return proveGitWorkInSource(ctx, refs, work.Path, work.HeadCommit, baseline.Commit)
}

// importGitViewCommit copies the objects the delivered HEAD adds over the
// baseline, as the daemon's verified walk lists them (listGitViewWork), from
// the view into the source object store, with no transport and no reads of
// the source checkout's configuration for routing. pack-objects packs exactly
// those objects in the pinned view and streams the pack straight into
// index-pack, which writes into the source object directory; --strict checks
// that every object the pack references resolves, the baseline objects being
// already present in the source.
//
// index-pack compares an object it receives with a loose copy the source
// already holds, but Git 2.54 does not compare it with a packed one, so a
// substitute planted in a pack would survive the import, shadowed by the new
// pack until pack order changes. Every copy the source already holds of the
// work's objects is therefore hashed first, and an alternates file the source
// gains meanwhile, which could let these checks pass on borrowed objects, is
// refused before and after.
func importGitViewCommit(ctx context.Context, baseline GitBaseline, result GitViewDelivery) error {
	refs, err := newGitSourceRefDir(ctx, filepath.Dir(result.Path), baseline)
	if err != nil {
		return err
	}
	defer refs.close()
	ids, err := listGitViewWork(ctx, result.Path, result.HeadCommit, baseline.Commit)
	if err != nil {
		return err
	}
	if err := refs.holdsNoSubstitutes(ctx, ids); err != nil {
		return err
	}
	// The pack holds exactly the verified listing, so no walk pack-objects
	// would make over the view's own objects decides what is imported.
	if err := pipeHostGit(ctx, 5*time.Minute,
		gitPipeEnd{dir: result.Path, env: gitViewEnv(result.Path),
			stdin: strings.NewReader(strings.Join(ids, "\n") + "\n"),
			args:  []string{"pack-objects", "--stdout", "--delta-base-offset", "-q"}},
		gitPipeEnd{dir: baseline.Root, env: gitSourceObjectEnv(baseline),
			args: []string{"index-pack", "--stdin", "--strict", "--fix-thin"}}); err != nil {
		return err
	}
	if err := gitSourceObjectIndirection(baseline.CommonDir); err != nil {
		return err
	}
	if _, err := gitSourceCommand(ctx, baseline, "cat-file", "-e", result.HeadCommit+"^{commit}"); err != nil {
		return fmt.Errorf("%w: delivered commit did not import into the source", ErrGitViewUnavailable)
	}
	return nil
}

// The source checkout may have advanced or contain another Run's uncommitted
// work. Delivery only imports objects and creates a separate ref, so requiring
// the old HEAD would make the second of two parallel writers undeliverable.
// Keep the repository/object identity and executable-config checks instead.
func inspectGitDeliverySource(ctx context.Context, baseline GitBaseline) error {
	root, err := filepath.EvalSymlinks(baseline.Root)
	if err != nil || filepath.Clean(root) != filepath.Clean(baseline.Root) {
		return fmt.Errorf("%w: source repository root changed", ErrGitViewUnavailable)
	}
	meta, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !meta.IsDir() {
		return fmt.Errorf("%w: source Git metadata changed", ErrGitViewUnavailable)
	}
	top, err := gitSourceProbe(ctx, root, "rev-parse", "--show-toplevel")
	if gitInterrupted(err) {
		return err
	}
	if err != nil || filepath.Clean(top) != root {
		return fmt.Errorf("%w: source repository topology changed", ErrGitViewUnavailable)
	}
	common, err := gitSourceProbe(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	resolvedCommon, err := filepath.EvalSymlinks(common)
	if err != nil || filepath.Clean(resolvedCommon) != filepath.Clean(baseline.CommonDir) {
		return fmt.Errorf("%w: source object directory changed", ErrGitViewUnavailable)
	}
	if _, err := gitSourceCommand(ctx, baseline, "cat-file", "-e", baseline.Commit+"^{commit}"); gitInterrupted(err) {
		return err
	} else if err != nil {
		return fmt.Errorf("%w: source baseline is unavailable", ErrGitViewUnavailable)
	}
	configKeys, err := gitSourceCommand(ctx, baseline, "config", "--list", "--name-only")
	if err != nil {
		return err
	}
	if err := screenGitSourceConfigKeys(configKeys); err != nil {
		return err
	}
	return gitSourceObjectIndirection(baseline.CommonDir)
}
