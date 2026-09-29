package executionenv

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// GitViewDelivery describes the immutable commit contribution and any work
// that still exists only in the managed checkout. The view itself is never
// removed or rewritten by inspection or delivery.
type GitViewDelivery struct {
	ViewID          string
	Path            string
	SourceRoot      string
	BaselineCommit  string
	HeadCommit      string
	CommittedFiles  int
	Uncommitted     int
	Untracked       int
	DeliveredBranch string
}

func InspectGitViewDelivery(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitViewDelivery, error) {
	view, err := InspectGitView(ctx, viewsDir, viewID, baseline)
	if err != nil {
		return GitViewDelivery{}, err
	}
	head, err := gitViewCommand(ctx, view.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return GitViewDelivery{}, err
	}
	changed, err := gitViewCommandRaw(ctx, view.Path, "diff", "--name-only", "-z", baseline.Commit, head)
	if err != nil {
		return GitViewDelivery{}, err
	}
	status, err := gitViewCommandRaw(ctx, view.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return GitViewDelivery{}, err
	}
	result := GitViewDelivery{ViewID: viewID, Path: view.Path, SourceRoot: baseline.Root,
		BaselineCommit: baseline.Commit, HeadCommit: head, CommittedFiles: strings.Count(changed, "\x00")}
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
	branch := "refs/heads/selfmind/" + viewID
	if existing, err := gitViewCommand(ctx, baseline.Root, "rev-parse", "--verify", branch+"^{commit}"); err == nil && existing == head {
		result.DeliveredBranch = strings.TrimPrefix(branch, "refs/heads/")
	}
	return result, nil
}

// DeliverGitViewBranch imports committed view work into a new, namespaced
// branch in the original repository. It never touches the checked-out branch,
// index, or worktree. A branch creation is atomic through update-ref's absent
// old-value check; a repeated request for the same commit is idempotent.
// Source or view drift fails before the branch is created. Uncommitted and
// untracked files remain in the view for separate review instead of being
// silently dropped from an apparently complete delivery.
func DeliverGitViewBranch(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitViewDelivery, error) {
	result, err := InspectGitViewDelivery(ctx, viewsDir, viewID, baseline)
	if err != nil {
		return GitViewDelivery{}, err
	}
	if result.Uncommitted != 0 || result.Untracked != 0 {
		return result, fmt.Errorf("%w: view has uncommitted or untracked work", ErrGitViewUnavailable)
	}
	if result.HeadCommit == baseline.Commit {
		return result, fmt.Errorf("%w: view has no committed changes to deliver", ErrGitViewUnavailable)
	}
	current, err := InspectCleanGitBaseline(ctx, baseline.Root)
	if err != nil || current != baseline {
		return result, fmt.Errorf("%w: source checkout changed or is dirty", ErrGitViewUnavailable)
	}
	branch := "refs/heads/selfmind/" + viewID
	if result.DeliveredBranch != "" {
		return result, nil
	}
	// The path was validated inside the daemon-owned views directory. Import
	// from that local repository only; no configured remote or network route
	// participates. This may add unreachable objects on failure, but it never
	// changes the source checkout or a named ref before the final CAS.
	if _, err := gitViewCommand(ctx, baseline.Root, "fetch", "--no-tags", "--no-write-fetch-head", "--", result.Path, "HEAD"); err != nil {
		return result, err
	}
	if _, err := gitViewCommand(ctx, baseline.Root, "cat-file", "-e", result.HeadCommit+"^{commit}"); err != nil {
		return result, err
	}
	current, err = InspectCleanGitBaseline(ctx, baseline.Root)
	if err != nil || current != baseline {
		return result, fmt.Errorf("%w: source checkout changed during delivery", ErrGitViewUnavailable)
	}
	viewNow, err := InspectGitViewDelivery(ctx, viewsDir, viewID, baseline)
	if err != nil || viewNow.HeadCommit != result.HeadCommit || viewNow.Uncommitted != 0 || viewNow.Untracked != 0 {
		return result, fmt.Errorf("%w: view changed during delivery", ErrGitViewUnavailable)
	}
	if _, err := gitViewCommand(ctx, baseline.Root, "update-ref", branch, result.HeadCommit, strings.Repeat("0", len(result.HeadCommit))); err != nil {
		// A concurrent identical delivery is success; a different branch tip
		// is a conflict and is never overwritten.
		if existing, checkErr := gitViewCommand(ctx, baseline.Root, "rev-parse", "--verify", branch+"^{commit}"); checkErr != nil || existing != result.HeadCommit {
			return result, fmt.Errorf("%w: delivery branch already changed", ErrGitViewUnavailable)
		}
	}
	result.DeliveredBranch = filepath.ToSlash(strings.TrimPrefix(branch, "refs/heads/"))
	return result, nil
}
