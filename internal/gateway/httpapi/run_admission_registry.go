package httpapi

import (
	"context"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

// admitActive shares the queue's resource check with direct admission. The
// registry mutex covers the decision and either publication or queue write;
// the worker pool still rechecks the physical roots at execution time.
func (c *RunCoordinator) admitActive(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, active *activeRun) (bool, api.MessageResponse, *notDispatched) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pendingSource := false
	if req.QueueID == "" {
		var err error
		pendingSource, err = c.srv.Control.HasPendingQueuedSource(ctx, identity.TenantID, identity.PersonID,
			req.Platform, req.PlatformUserID, req.Channel)
		if err != nil {
			return false, api.MessageResponse{Identity: identity, Error: refusalRecords,
					Turn: messageTurn("failed", "", "idle", "", "", refusalRecords)},
				&notDispatched{reason: refusalRecords, cause: err}
		}
	}
	held := make([]*activeRun, 0, len(c.active[identity.PersonID]))
	for run := range c.active[identity.PersonID] {
		held = append(held, run)
	}
	reservations, err := c.srv.Control.ListRunRootReservations(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return false, api.MessageResponse{Identity: identity, Error: refusalRecords,
				Turn: messageTurn("failed", "", "idle", "", "", refusalRecords)},
			&notDispatched{reason: refusalRecords, cause: err}
	}
	ready := queuedResourcesReady(control.QueuedTask{Platform: req.Platform, Channel: req.Channel,
		ReplyToRunID: req.ReplyToRunID, ApprovalID: req.ApprovalID, ClarifyID: req.ClarifyID, ExecutionRoots: req.ExecutionRoots}, held, reservations)
	if !pendingSource && ready && c.beginActiveLocked(identity.PersonID, active) {
		return true, api.MessageResponse{}, nil
	}
	if req.QueueID != "" {
		// A drain keeps ownership of its exact row and releases an unbound
		// claim after a lost admission race. Never make a second queue copy.
		return false, api.MessageResponse{Identity: identity,
			Turn: messageTurn("capacity_wait", "queued", "idle", req.TaskID, "", "capacity or execution roots are occupied")}, nil
	}
	response, refusal := c.srv.enqueueBehindActiveLocked(ctx, identity, req)
	return false, response, refusal
}

type asyncAdmissionContextKey struct{}

// Publish the selected physical view before Run insertion so retention sees
// the actual directory throughout the remaining admission window.
func (c *RunCoordinator) setActiveExecutionRoots(ctx context.Context, roots []executionenv.RootBinding) {
	active, _ := ctx.Value(activeRunContextKey{}).(*activeRun)
	if active == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, registered := c.active[active.PersonID][active]; registered {
		active.ExecutionRoots = executionenv.CloneRootBindings(roots)
	}
}

// Only successful durable Run insertion acknowledges background execution.
// An earlier failure is returned by startAsyncRun through the same signal.
func (c *RunCoordinator) signalAsyncAdmission(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run) {
	if signal, _ := ctx.Value(asyncAdmissionContextKey{}).(chan api.MessageResponse); signal != nil {
		select {
		case signal <- api.MessageResponse{Identity: identity, Accepted: true,
			Turn: messageTurn("accepted", "running", "running", task.ID, run.ID, "")}:
		default:
		}
	}
}
