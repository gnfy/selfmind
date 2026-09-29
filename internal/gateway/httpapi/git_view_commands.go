package httpapi

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

func (d *Server) ownedGitView(ctx context.Context, identity *control.IdentityContext, runID string) (*control.Run, executionenv.RootBinding, error) {
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
	viewID := filepath.Base(root.Path)
	view, err := executionenv.InspectGitView(ctx, d.Control.ExecutionViewsDir(), viewID, *root.GitBaseline)
	if err != nil || filepath.Clean(root.Path) != view.Path {
		return nil, executionenv.RootBinding{}, fmt.Errorf("execution view is unavailable or changed; its files were preserved")
	}
	return run, root, nil
}

func (d *Server) gitViewsReply(ctx context.Context, identity *control.IdentityContext, runID string) (string, error) {
	if strings.TrimSpace(runID) != "" {
		run, root, err := d.ownedGitView(ctx, identity, runID)
		if err != nil {
			return "", err
		}
		info, err := executionenv.InspectGitViewDelivery(ctx, d.Control.ExecutionViewsDir(), filepath.Base(root.Path), *root.GitBaseline)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Execution view for run %s\nPath: %s\nSource: %s\nCommitted files: %d\nUncommitted changes: %d\nUntracked files: %d\nDelivered branch: %s\nUse /apply %s to deliver a clean committed view as a separate branch.",
			run.ID, info.Path, info.SourceRoot, info.CommittedFiles, info.Uncommitted,
			info.Untracked, fallback(info.DeliveredBranch, "none"), run.ID), nil
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
