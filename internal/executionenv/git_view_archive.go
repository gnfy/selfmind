package executionenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// GitViewMoveGuard runs the one rename that takes a checkout out of its Run's
// execution path. A caller that admits Runs supplies a guard that rechecks
// admission atomically with the rename and returns its refusal instead of
// calling move; nil performs the rename directly.
type GitViewMoveGuard func(move func() error) error

func (guard GitViewMoveGuard) run(move func() error) error {
	if guard == nil {
		return move()
	}
	return guard(move)
}

// RetireGitView moves a delivered, clean view into daemon-owned retention.
// It never deletes bytes, including Git metadata and ignored files. The
// original Run root is deliberately left missing, so an unexpected resume
// fails closed until the person explicitly restores the view.
func RetireGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline, guard GitViewMoveGuard) (GitView, error) {
	if !safeGitViewID(viewID) {
		return GitView{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	if _, err := os.Lstat(filepath.Join(viewsDir, viewID)); os.IsNotExist(err) {
		archiveDir, err := retiredGitViewDir(viewsDir)
		if err != nil {
			return GitView{}, err
		}
		return InspectGitView(ctx, archiveDir, viewID, baseline)
	} else if err != nil {
		return GitView{}, err
	}
	info, err := InspectGitViewDelivery(ctx, viewsDir, viewID, baseline)
	if err != nil {
		return GitView{}, err
	}
	if err := gitViewRetentionBlocker(info, baseline); err != nil {
		return GitView{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Clean(viewsDir))
	if err != nil {
		return GitView{}, err
	}
	archivePath := filepath.Join(parent, ".retired")
	if err := os.MkdirAll(archivePath, 0o700); err != nil {
		return GitView{}, err
	}
	archive, err := retiredGitViewDir(viewsDir)
	if err != nil {
		return GitView{}, err
	}
	retired := filepath.Join(archive, viewID)
	if _, err := os.Lstat(retired); err == nil {
		return GitView{}, fmt.Errorf("%w: retired view path already exists", ErrGitViewUnavailable)
	} else if !os.IsNotExist(err) {
		return GitView{}, err
	}
	if err := guard.run(func() error { return os.Rename(info.Path, retired) }); err != nil {
		return GitView{}, err
	}
	return InspectGitView(ctx, archive, viewID, baseline)
}

// RestoreGitView reverses retirement without creating a checkout from the
// baseline. A missing or changed retained view is never silently rebuilt.
func RestoreGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitView, error) {
	if !safeGitViewID(viewID) {
		return GitView{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	live := filepath.Join(viewsDir, viewID)
	if _, err := os.Lstat(live); err == nil {
		archiveDir, archiveErr := retiredGitViewDir(viewsDir)
		if archiveErr == nil {
			if _, duplicateErr := os.Lstat(filepath.Join(archiveDir, viewID)); duplicateErr == nil {
				return GitView{}, fmt.Errorf("%w: both live and retired views exist", ErrGitViewUnavailable)
			} else if !os.IsNotExist(duplicateErr) {
				return GitView{}, duplicateErr
			}
		} else if !errors.Is(archiveErr, os.ErrNotExist) {
			return GitView{}, archiveErr
		}
		return InspectGitView(ctx, viewsDir, viewID, baseline)
	} else if !os.IsNotExist(err) {
		return GitView{}, err
	}
	archiveDir, err := retiredGitViewDir(viewsDir)
	if err != nil {
		return GitView{}, err
	}
	retired := filepath.Join(archiveDir, viewID)
	if _, err := InspectGitView(ctx, archiveDir, viewID, baseline); err != nil {
		return GitView{}, err
	}
	if err := os.Rename(retired, live); err != nil {
		return GitView{}, err
	}
	return InspectGitView(ctx, viewsDir, viewID, baseline)
}

// gitViewRetentionBlocker explains why a view cannot leave its Run's execution
// path without hiding work. Archive and prune share it; prune adds the object
// and reference proofs because it deletes the checkout.
func gitViewRetentionBlocker(info GitViewDelivery, baseline GitBaseline) error {
	switch {
	case !info.WorktreeInspected:
		return fmt.Errorf("%w: view checkout was not inspected", ErrGitViewUnavailable)
	case info.Uncommitted != 0 || info.Untracked != 0:
		return fmt.Errorf("%w: view has uncommitted or untracked work", ErrGitViewUnavailable)
	case info.Ignored != 0:
		return fmt.Errorf("%w: view has ignored files that would leave with it", ErrGitViewUnavailable)
	case info.HiddenIndexEntries != 0:
		return fmt.Errorf("%w: view has %d index entries marked skip-worktree or assume-unchanged (or unmerged), so git status cannot show their edits; clear those bits and review the files first",
			ErrGitViewUnavailable, info.HiddenIndexEntries)
	case info.LinkedWorktrees != 0:
		return fmt.Errorf("%w: view has %d linked Git worktrees under .git/worktrees whose files and commits are outside its status; remove them after recovering their work",
			ErrGitViewUnavailable, info.LinkedWorktrees)
	case info.Submodules != 0:
		return fmt.Errorf("%w: view contains submodule repositories whose work git status does not inspect", ErrGitViewUnavailable)
	case info.HeadCommit != baseline.Commit && info.DeliveredBranch == "":
		return fmt.Errorf("%w: view has committed work that the source repository does not hold as a delivered branch; deliver it with /apply", ErrGitViewUnavailable)
	}
	return nil
}

func InspectRetiredGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitView, error) {
	archiveDir, err := retiredGitViewDir(viewsDir)
	if err != nil {
		return GitView{}, err
	}
	return InspectGitView(ctx, archiveDir, viewID, baseline)
}

func retiredGitViewDir(viewsDir string) (string, error) {
	if !filepath.IsAbs(viewsDir) {
		return "", fmt.Errorf("%w: invalid view directory", ErrGitViewUnavailable)
	}
	parent, err := filepath.EvalSymlinks(filepath.Clean(viewsDir))
	if err != nil {
		return "", err
	}
	archive := filepath.Join(parent, ".retired")
	info, err := os.Lstat(archive)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %w", ErrGitViewUnavailable, os.ErrNotExist)
	}
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: retained view directory is unavailable", ErrGitViewUnavailable)
	}
	return archive, nil
}
