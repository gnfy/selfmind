package httpapi

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/log"
)

// maybeAssignIsolatedGitView gives a second independent writer its own
// checkout only when the first direct writer froze the same clean baseline.
// Any uncertainty retains physical-root serialization. It runs before Run
// insertion, so the durable Run and every later tool see the selected path.
func (c *RunCoordinator) maybeAssignIsolatedGitView(ctx context.Context, identity *control.IdentityContext, task *control.Task, req *api.MessageRequest, hasParent bool) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || req == nil || hasParent ||
		c.activeCapacity() < 2 || len(req.ExecutionRoots) != 1 || strings.TrimSpace(req.ReplyToRunID) != "" {
		return
	}
	root := req.ExecutionRoots[0]
	if root.Source != executionenv.RootSourceWorkspace || root.Role != executionenv.RootRolePrimary || root.GitBaseline == nil || !root.Writable() {
		return
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
		return
	}
	viewsDir := c.srv.Control.ExecutionViewsDir()
	if viewsDir == "" {
		return
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
		log.Warn("gateway: could not reserve isolated Git view; retaining physical root lock", "error", err)
		return
	}
	view, err := executionenv.EnsureGitView(ctx, viewsDir, viewID, *root.GitBaseline)
	if err != nil {
		log.Warn("gateway: could not materialize isolated Git view; retaining physical root lock", "view_id", viewID, "error", err)
		return
	}
	root.Path = view.Path
	root.Source = executionenv.RootSourceExecutionView
	root.ContextRoot = true
	req.ExecutionRoots = []executionenv.RootBinding{root}
}
