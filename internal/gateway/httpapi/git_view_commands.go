package httpapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/runpool"
	"selfmind/internal/tools"
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

// gitViewWriter names what may still change a view: the owning Run, another
// durable running Run bound to it, or an admission that registered it and has
// not finished. Host-side Git never reads that checkout meanwhile.
func (d *Server) gitViewWriter(ctx context.Context, identity *control.IdentityContext, run *control.Run, path string) (string, error) {
	if run.Status == "running" {
		return "the run is still writing its execution view", nil
	}
	active, err := d.Control.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
	if err != nil {
		return "", err
	}
	for _, other := range active {
		for _, binding := range other.ExecutionRoots {
			if filepath.Clean(binding.Path) == filepath.Clean(path) {
				return "another active run is still writing this execution view", nil
			}
		}
	}
	if d.coordinator().executionViewAdmitted(path) {
		return "a Run is starting in this execution view", nil
	}
	return "", nil
}

// gitSourceWriter names what may still change the admitted source checkout
// while /apply or /views prune works in it: a Run being admitted or running
// with write access to it, a parked Run that can resume there, or a background
// process such a Run started that is still alive. Ref creation itself reads no
// source configuration, but delivery and prune still read the checkout's refs
// and objects; a writer must not change them mid-operation, so the person is
// told which Run or process to wait for. A child that escaped a sandboxed
// command without being registered is not visible here.
func (d *Server) gitSourceWriter(ctx context.Context, identity *control.IdentityContext, baseline executionenv.GitBaseline) (string, error) {
	targets := []string{baseline.Root, baseline.CommonDir}
	writes := func(roots []executionenv.RootBinding) bool {
		return runpool.PathsConflict(targets, writableRootPaths(roots))
	}
	for _, active := range d.coordinator().activeRunsForPerson(identity.PersonID) {
		if !writes(active.ExecutionRoots) {
			continue
		}
		if active.RunID == "" {
			return fmt.Sprintf("a Run is starting with write access to the source checkout %s", baseline.Root), nil
		}
		return fmt.Sprintf("Run %s is running with write access to the source checkout %s", active.RunID, baseline.Root), nil
	}
	unfinished, err := d.Control.ListUnfinishedRunRoots(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "", err
	}
	for _, run := range unfinished {
		if writes(run.Roots) {
			return fmt.Sprintf("Run %s (%s) can still write the source checkout %s", run.RunID, run.Status, baseline.Root), nil
		}
	}
	for _, process := range tools.LiveBackgroundProcesses(ctx, identity.PersonID) {
		runID := process.RunID
		if runID == "" && process.LeaseID != "" {
			if lease, err := d.Control.GetExecutionLease(ctx, identity.TenantID, process.LeaseID); err == nil && lease != nil {
				runID = lease.RunID
			}
		}
		if runID == "" {
			// Recorded by an earlier daemon: only the working directory remains.
			if process.CWD != "" && runpool.PathsConflict(targets, []string{process.CWD}) {
				return fmt.Sprintf("background process %s (%s) is still running in the source checkout %s",
					process.ID, truncate(process.Command, 80), baseline.Root), nil
			}
			continue
		}
		run, err := d.Control.GetRun(ctx, identity.TenantID, runID)
		if err != nil {
			return "", err
		}
		if run != nil && run.PersonID == identity.PersonID && writes(run.ExecutionRoots) {
			return fmt.Sprintf("background process %s (%s) started by Run %s is still running with write access to the source checkout %s",
				process.ID, truncate(process.Command, 80), runID, baseline.Root), nil
		}
	}
	return "", nil
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
		writer, err := d.gitViewWriter(ctx, identity, run, root.Path)
		if err != nil {
			return "", err
		}
		inspect := executionenv.InspectGitViewDelivery
		if writer != "" {
			inspect = executionenv.InspectGitViewCommits
		}
		info, err := inspect(ctx, viewsDir, filepath.Base(root.Path), *root.GitBaseline)
		if err != nil {
			return "", err
		}
		checkout := "Checkout: not inspected while a Run may still write this view\n"
		if info.WorktreeInspected {
			checkout = fmt.Sprintf("Uncommitted changes: %d\nUntracked files: %d\nIgnored files: %d\n", info.Uncommitted, info.Untracked, info.Ignored)
			if hidden := hiddenGitViewState(info); hidden != "" {
				checkout += "Hidden from git status: " + hidden + "\n"
			}
		}
		return fmt.Sprintf("Execution view for run %s\nPath: %s\nSource: %s\nCommitted files: %d\n%sDelivered branch: %s\nRetired: %t\nUse /apply %s to deliver a clean committed view as a separate branch (restore it first if retired). /views archive and restore preserve all files. /views prune permanently removes only a proven-safe archived checkout while retaining its commit in the source repository.",
			run.ID, info.Path, info.SourceRoot, info.CommittedFiles, checkout,
			fallback(info.DeliveredBranch, "none"), retired, run.ID), nil
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

func hiddenGitViewState(info executionenv.GitViewDelivery) string {
	var parts []string
	if info.HiddenIndexEntries != 0 {
		parts = append(parts, fmt.Sprintf("%d skip-worktree, assume-unchanged, or unmerged index entries", info.HiddenIndexEntries))
	}
	if info.LinkedWorktrees != 0 {
		parts = append(parts, fmt.Sprintf("%d linked worktrees", info.LinkedWorktrees))
	}
	if info.Submodules != 0 {
		parts = append(parts, fmt.Sprintf("%d submodule repositories", info.Submodules))
	}
	return strings.Join(parts, ", ")
}

// retainableGitView is the first, cheap half of a retention check. The move
// guard repeats it atomically with the rename after the slow inspection.
func (d *Server) retainableGitView(ctx context.Context, identity *control.IdentityContext, path string) (control.ExecutionViewUsage, error) {
	usage, err := d.Control.ExecutionViewUsage(ctx, identity.TenantID, identity.PersonID, path)
	if err != nil {
		return control.ExecutionViewUsage{}, err
	}
	if usage.Pending {
		return control.ExecutionViewUsage{}, errExecutionViewPending
	}
	if d.coordinator().executionViewAdmitted(path) {
		return control.ExecutionViewUsage{}, errExecutionViewAdmitting
	}
	return usage, nil
}

func (d *Server) pruneGitViewReply(ctx context.Context, identity *control.IdentityContext, runID string) (string, error) {
	run, root, err := d.ownedGitViewRoot(ctx, identity, runID)
	if err != nil {
		return "", err
	}
	if run.Status != "done" {
		return "", fmt.Errorf("only a completed Run's archived view can be pruned")
	}
	usage, err := d.retainableGitView(ctx, identity, root.Path)
	if err != nil {
		return "", err
	}
	if blocker, err := d.gitSourceWriter(ctx, identity, *root.GitBaseline); err != nil {
		return "", err
	} else if blocker != "" {
		return "", fmt.Errorf("%s; /views prune waits for it to end because pruning writes a protected ref in that checkout", blocker)
	}
	pruned, err := executionenv.PruneRetiredGitView(ctx, d.Control.ExecutionViewsDir(), filepath.Base(root.Path), *root.GitBaseline,
		d.coordinator().executionViewMoveGuard(ctx, identity, root.Path, usage))
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
	viewID := filepath.Base(root.Path)
	if restore {
		// Restoring puts the same bytes back on the path its Runs expect. A
		// parked or queued Run that refers to the view is exactly the work
		// that needs it, so neither its status nor its references block this.
		view, err := executionenv.RestoreGitView(ctx, d.Control.ExecutionViewsDir(), viewID, *root.GitBaseline)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Restored execution view for run %s at %s. No work was discarded.", run.ID, view.Path), nil
	}
	if run.Status != "done" {
		return "", fmt.Errorf("only a completed Run's view can be retired")
	}
	usage, err := d.retainableGitView(ctx, identity, root.Path)
	if err != nil {
		return "", err
	}
	view, err := executionenv.RetireGitView(ctx, d.Control.ExecutionViewsDir(), viewID, *root.GitBaseline,
		d.coordinator().executionViewMoveGuard(ctx, identity, root.Path, usage))
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
	writer, err := d.gitViewWriter(ctx, identity, run, root.Path)
	if err != nil {
		return "", err
	}
	if writer != "" {
		return "", errors.New(writer)
	}
	if blocker, err := d.gitSourceWriter(ctx, identity, *root.GitBaseline); err != nil {
		return "", err
	} else if blocker != "" {
		return "", fmt.Errorf("%s; /apply waits for it to end because delivery writes a branch in that checkout", blocker)
	}
	info, err := executionenv.DeliverGitViewBranch(ctx, d.Control.ExecutionViewsDir(), filepath.Base(root.Path), *root.GitBaseline)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Delivered run %s as branch %s in %s. The current checkout was not changed; the execution view remains at %s for review.",
		run.ID, info.DeliveredBranch, info.SourceRoot, info.Path), nil
}
