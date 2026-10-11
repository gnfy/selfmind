package httpapi

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/log"
	"selfmind/internal/runpool"
)

// maybeAssignIsolatedGitView gives a second independent writer its own
// checkout only when the first direct writer froze the same clean baseline.
// A needed view that cannot be prepared returns false so the caller queues
// the work instead of admitting another writer to the original root. It runs before Run
// insertion, so the durable Run and every later tool see the selected path.
func (c *RunCoordinator) maybeAssignIsolatedGitView(ctx context.Context, identity *control.IdentityContext, task *control.Task, req *api.MessageRequest, hasParent bool) bool {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || req == nil || hasParent ||
		c.activeCapacity() < 2 || len(req.ExecutionRoots) != 1 || strings.TrimSpace(req.ReplyToRunID) != "" {
		return true
	}
	root := req.ExecutionRoots[0]
	if root.Source != executionenv.RootSourceWorkspace || root.Role != executionenv.RootRolePrimary || root.GitBaseline == nil || !root.Writable() {
		return true
	}
	var directWriter bool
	for _, active := range c.activeRunsForPerson(identity.PersonID) {
		if active.RunID == "" || len(active.ExecutionRoots) != 1 {
			continue
		}
		held := active.ExecutionRoots[0]
		if held.Path == root.Path && held.Source == executionenv.RootSourceWorkspace && held.Writable() && held.GitBaseline != nil && *held.GitBaseline == *root.GitBaseline {
			directWriter = true
		}
	}
	if !directWriter {
		return true
	}
	viewsDir := c.srv.Control.ExecutionViewsDir()
	if viewsDir == "" {
		return false
	}
	viewID := "view_" + uuid.NewString()
	if req.QueueID != "" {
		viewID = "view_" + req.QueueID
	}
	// Reserve an inspectable identity before the Git side effect. A crash
	// after materialization but before Run insertion leaves an orphan to
	// quarantine, never an untracked directory we can silently reuse.
	if _, err := c.srv.Control.AppendEvent(ctx, control.Event{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Type: "execution.view.reserved", Visibility: "internal", Channel: req.Channel,
		IdempotencyKey: "execution-view-reserved:" + viewID,
		Payload:        mustJSON(map[string]interface{}{"view_id": viewID, "baseline": root.GitBaseline}),
	}); err != nil {
		log.Warn("gateway: could not reserve isolated Git view; keeping work queued", "error", err)
		return false
	}
	view, err := executionenv.EnsureGitView(ctx, viewsDir, viewID, *root.GitBaseline)
	if err != nil {
		log.Warn("gateway: could not materialize isolated Git view; keeping work queued", "view_id", viewID, "error", err)
		return false
	}
	root.Path = view.Path
	root.Source = executionenv.RootSourceExecutionView
	root.ContextRoot = true
	req.ExecutionRoots = []executionenv.RootBinding{root}
	return true
}

// View retention and Run admission meet at the coordinator registry. Every
// admission registers its frozen execution roots there (beginActive, under
// c.mu) before it inserts the durable Run, and stays registered while the Run
// executes. A retention move holds the same mutex across a registry check, a
// durable usage re-check, and the rename, so the two cannot interleave: either
// the admission is visible and the move is refused, or the move finishes
// first and the admitted Run finds the view archived and parks with that
// explanation. Lock order is c.mu, then the control store; no path takes c.mu
// while it holds the store's connection.
var (
	errExecutionViewAdmitting       = errors.New("a Run is starting or running in this execution view; the view was left in place")
	errExecutionViewPending         = errors.New("another unfinished Run or queued item still refers to this view")
	errExecutionViewUsedDuringCheck = errors.New("a Run used this execution view while it was being checked; the view was left in place, so run the command again")
)

func (c *RunCoordinator) executionViewAdmitted(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executionViewAdmittedLocked(path)
}

func (c *RunCoordinator) executionViewAdmittedLocked(path string) bool {
	path = filepath.Clean(path)
	for _, runs := range c.active {
		for active := range runs {
			for _, root := range active.ExecutionRoots {
				if runpool.PathsConflict([]string{root.Path}, []string{path}) {
					return true
				}
			}
		}
	}
	return false
}

// executionViewMoveGuard refuses the rename unless the view still has exactly
// the durable usage the caller inspected: no admission in flight, no
// unfinished Run or queued item, and no Run bound since the first check. The
// last condition catches a Run that started, wrote, and finished while the
// slow Git inspection ran.
func (c *RunCoordinator) executionViewMoveGuard(ctx context.Context, identity *control.IdentityContext, path string, checked control.ExecutionViewUsage) executionenv.GitViewMoveGuard {
	return func(move func() error) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.executionViewAdmittedLocked(path) {
			return errExecutionViewAdmitting
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		current, err := c.srv.Control.ExecutionViewUsage(readCtx, identity.TenantID, identity.PersonID, path)
		if err != nil {
			return err
		}
		if current.Pending {
			return errExecutionViewPending
		}
		if !slices.Equal(current.RunIDs, checked.RunIDs) {
			return errExecutionViewUsedDuringCheck
		}
		return move()
	}
}
