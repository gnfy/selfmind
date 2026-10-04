package executionenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RetireGitView moves a delivered, clean view into daemon-owned retention.
// It never deletes bytes, including Git metadata and ignored files. The
// original Run root is deliberately left missing, so an unexpected resume
// fails closed until the person explicitly restores the view.
func RetireGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (GitView, error) {
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
	if info.Uncommitted != 0 || info.Untracked != 0 || info.Ignored != 0 ||
		info.HeadCommit != baseline.Commit && info.DeliveredBranch == "" {
		return GitView{}, fmt.Errorf("%w: view has undelivered or uncommitted work", ErrGitViewUnavailable)
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
	if err := os.Rename(info.Path, retired); err != nil {
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
