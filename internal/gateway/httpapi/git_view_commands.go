package httpapi

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

func (d *Server) ownedGitViewRoot(ctx context.Context, identity *control.IdentityContext, runID string) (*control.Run, executionenv.RootBinding, error) {
	if d == nil || d.Control == nil || identity == nil || strings.TrimSpace(runID) == "" {
		return nil, executionenv.RootBinding{}, fmt.Errorf("exact run id is required")
	}
	run, err := d.Control.GetRun(ctx, identity.TenantID, strings.TrimSpace(runID))
	if err != nil {
		return nil, executionenv.RootBinding{}, err
	}
	if run == nil || run.PersonID != identity.PersonID {
		return nil, executionenv.RootBinding{}, fmt.Errorf("execution view is not available for this run")
	}
	if len(run.ExecutionRoots) != 1 {
		return nil, executionenv.RootBinding{}, fmt.Errorf("this run has no independent execution view")
	}
	root := run.ExecutionRoots[0]
	if root.Source != executionenv.RootSourceExecutionView || root.GitBaseline == nil {
		return nil, executionenv.RootBinding{}, fmt.Errorf("this run has no independent execution view")
	}
	viewsDir, err := filepath.EvalSymlinks(d.Control.ExecutionViewsDir())
	if err != nil || filepath.Join(viewsDir, filepath.Base(root.Path)) != filepath.Clean(root.Path) {
		return nil, executionenv.RootBinding{}, fmt.Errorf("execution view path is unavailable or changed")
	}
	return run, root, nil
}

func (d *Server) ownedGitView(ctx context.Context, identity *control.IdentityContext, runID string) (*control.Run, executionenv.RootBinding, error) {
	run, root, err := d.ownedGitViewRoot(ctx, identity, runID)
	if err != nil {
		return nil, executionenv.RootBinding{}, err
	}
	viewID := filepath.Base(root.Path)
	view, err := executionenv.InspectGitView(ctx, d.Control.ExecutionViewsDir(), viewID, *root.GitBaseline)
	if err != nil || filepath.Clean(root.Path) != view.Path {
		return nil, executionenv.RootBinding{}, fmt.Errorf("execution view is unavailable or changed; its files were preserved")
	}
	return run, root, nil
}

func (d *Server) gitViewsReply(ctx context.Context, identity *control.IdentityContext, runID string) (string, error) {
	if strings.TrimSpace(runID) != "" {
		run, root, err := d.ownedGitViewRoot(ctx, identity, runID)
		if err != nil {
			return "", err
		}
		viewsDir := d.Control.ExecutionViewsDir()
		retired := false
		if _, inspectErr := executionenv.InspectGitView(ctx, viewsDir, filepath.Base(root.Path), *root.GitBaseline); inspectErr != nil {
			if _, retiredErr := executionenv.InspectRetiredGitView(ctx, viewsDir, filepath.Base(root.Path), *root.GitBaseline); retiredErr != nil {
				if pruned, pruneErr := executionenv.InspectPrunedGitView(ctx, viewsDir, filepath.Base(root.Path), *root.GitBaseline); pruneErr == nil {
					return fmt.Sprintf("Execution view for run %s was pruned after delivery. Committed work remains at %s in %s; the checkout cannot be restored.",
						run.ID, pruned.RetainedRef, pruned.SourceRoot), nil
				}
				return "", fmt.Errorf("execution view is unavailable or changed; its files were preserved")
			}
			viewsDir = filepath.Join(viewsDir, ".retired")
			retired = true
		}
		info, err := executionenv.InspectGitViewDelivery(ctx, viewsDir, filepath.Base(root.Path), *root.GitBaseline)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Execution view for run %s\nPath: %s\nSource: %s\nCommitted files: %d\nUncommitted changes: %d\nUntracked files: %d\nIgnored files: %d\nDelivered branch: %s\nRetired: %t\nUse /apply %s to deliver a clean committed view as a separate branch (restore it first if retired). /views archive and restore preserve all files. /views prune permanently removes only a proven-safe archived checkout while retaining its commit in the source repository.",
			run.ID, info.Path, info.SourceRoot, info.CommittedFiles, info.Uncommitted,
			info.Untracked, info.Ignored, fallback(info.DeliveredBranch, "none"), retired, run.ID), nil
	}
	recent, err := d.Control.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 100)
	if err != nil {
		return "", err
	}
	var lines []string
	seen := map[string]bool{}
	for _, digest := range recent {
		run, err := d.Control.GetRun(ctx, identity.TenantID, digest.RunID)
		if err != nil || run == nil || len(run.ExecutionRoots) != 1 {
			continue
		}
		root := run.ExecutionRoots[0]
		if root.Source != executionenv.RootSourceExecutionView || seen[root.Path] {
			continue
		}
		seen[root.Path] = true
		lines = append(lines, fmt.Sprintf("- %s | %s | %s", run.ID, digest.TaskTitle, run.Status))
		if len(lines) == 10 {
			break
		}
	}
	if len(lines) == 0 {
		return "No execution views for recent runs.", nil
	}
	return "Execution views:\n" + strings.Join(lines, "\n") + "\nOpen: /views <run_id>", nil
}

func (d *Server) pruneGitViewReply(ctx context.Context, identity *control.IdentityContext, runID string) (string, error) {
	run, root, err := d.ownedGitViewRoot(ctx, identity, runID)
	if err != nil {
		return "", err
	}
	if run.Status != "done" {
		return "", fmt.Errorf("only a completed Run's archived view can be pruned")
	}
	busy, err := d.Control.ExecutionViewReferencedByPendingWork(ctx, identity.TenantID, identity.PersonID, root.Path)
	if err != nil {
		return "", err
	}
	if busy {
		return "", fmt.Errorf("another unfinished Run or queued item still refers to this view")
	}
	pruned, err := executionenv.PruneRetiredGitView(ctx, d.Control.ExecutionViewsDir(), filepath.Base(root.Path), *root.GitBaseline)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Pruned archived execution view for run %s. Committed work remains protected at %s in %s. The checkout and its local Git metadata cannot be restored.",
		run.ID, pruned.RetainedRef, pruned.SourceRoot), nil
}

func (d *Server) archiveGitViewReply(ctx context.Context, identity *control.IdentityContext, runID string, restore bool) (string, error) {
	run, root, err := d.ownedGitViewRoot(ctx, identity, runID)
	if err != nil {
		return "", err
	}
	if run.Status != "done" {
		return "", fmt.Errorf("only a completed Run's view can be retired or restored")
	}
	busy, err := d.Control.ExecutionViewReferencedByPendingWork(ctx, identity.TenantID, identity.PersonID, root.Path)
	if err != nil {
		return "", err
	}
	if busy {
		return "", fmt.Errorf("another unfinished Run or queued item still refers to this view")
	}
	viewID := filepath.Base(root.Path)
	if restore {
		view, err := executionenv.RestoreGitView(ctx, d.Control.ExecutionViewsDir(), viewID, *root.GitBaseline)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Restored execution view for run %s at %s. No work was discarded.", run.ID, view.Path), nil
	}
	view, err := executionenv.RetireGitView(ctx, d.Control.ExecutionViewsDir(), viewID, *root.GitBaseline)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Retired execution view for run %s at %s. All files and Git metadata remain available; use /views restore %s to move it back.",
		run.ID, view.Path, run.ID), nil
}

func (d *Server) applyGitViewReply(ctx context.Context, identity *control.IdentityContext, runID string) (string, error) {
	run, root, err := d.ownedGitView(ctx, identity, runID)
	if err != nil {
		return "", err
	}
	if run.Status == "running" {
		return "", fmt.Errorf("the run is still writing its execution view")
	}
	active, err := d.Control.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
	if err != nil {
		return "", err
	}
	for _, other := range active {
		for _, binding := range other.ExecutionRoots {
			if binding.Path == root.Path {
				return "", fmt.Errorf("another active run is still writing this execution view")
			}
		}
	}
	info, err := executionenv.DeliverGitViewBranch(ctx, d.Control.ExecutionViewsDir(), filepath.Base(root.Path), *root.GitBaseline)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Delivered run %s as branch %s in %s. The current checkout was not changed; the execution view remains at %s for review.",
		run.ID, info.DeliveredBranch, info.SourceRoot, info.Path), nil
}
