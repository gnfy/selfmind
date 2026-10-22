package httpapi

import (
	"context"
	"fmt"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"strings"
	"time"
)

func (c *RunCoordinator) parkExecutionWait(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, req api.MessageRequest, toolName string, host bool) (parked bool, err error) {
	defer func() {
		if err != nil {
			_, _ = c.srv.Control.AppendEvent(context.WithoutCancel(ctx), control.Event{RunID: run.ID, Type: "checkpoint.failed", Visibility: "task", Payload: mustJSON(map[string]interface{}{"outcome": "await_execution", "worker_retained": true, "error": err.Error()})})
		}
	}()
	if err := kernel.SaveExecutionWaitCheckpoint(ctx, run.ID, toolName, host); err != nil {
		return false, err
	}
	route := c.srv.routeIdentityForPerson(ctx, identity.TenantID, identity.PersonID, req.Channel, req.Platform, identity)
	if route == nil || route.PersonID != identity.PersonID || route.PlatformUserID == "" {
		return false, fmt.Errorf("execution wait origin cannot be routed")
	}
	outcome := api.RunOutcome{Status: "waiting_external", CompletionReason: "execution_lane_wait", Summary: "Waiting for this person's managed execution lane. This tool call was not dispatched.", NextSteps: []string{"SelfMind will resume the exact pending call when execution is available."}}
	queued := control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: route.Platform, PlatformUserID: route.PlatformUserID, Channel: req.Channel,
		Content: "Continue the exact pending tool call after the managed execution lane became available. Preserve completed tool results; do not repeat completed effects.", WorkspaceID: run.WorkspaceID, ExecutionRoots: executionenv.CloneRootBindings(run.ExecutionRoots), TaskID: task.ID, ReplyToRunID: run.ID, IdempotencyKey: "execution-lane:" + run.ID, Class: control.QueueClassFinalization, NotBefore: time.Now().Truncate(time.Second), Admission: req.Admission}
	_, err = c.srv.Control.MaterializeRunFinalization(context.WithoutCancel(ctx), control.RunFinalization{Identity: *identity, RunID: run.ID, RunStatus: "waiting_external", TaskID: task.ID, TaskStatus: "in_progress", Summary: outcome.Summary, NextSteps: outcome.NextSteps, Channel: req.Channel, Handoff: control.Handoff{TaskID: task.ID, Summary: outcome.Summary, NextSteps: outcome.NextSteps}, Event: control.Event{Type: "run.finished", Visibility: "task", Channel: req.Channel, Payload: mustJSON(map[string]interface{}{"outcome": outcome})}, Continuation: &queued, ExpectedRunStatus: "running", RequireCheckpoint: true, ConsumedQueueID: req.QueueID, ConsumedQueueClaimToken: req.QueueClaimToken})
	if err != nil {
		return false, err
	}
	run.Status = "waiting_external"
	kernel.MarkExecutionWaitParked(ctx)
	if host && c.srv.ExecutionRuntime != nil {
		c.srv.ExecutionRuntime.PreserveQueuedHostExecution(identity.TenantID, identity.PersonID, run.ID)
	}
	return true, nil
}
func (c *RunCoordinator) queuedExecutionLaneReady(q control.QueuedTask, hosts map[string]bool) bool {
	if !strings.HasPrefix(q.IdempotencyKey, "execution-lane:") {
		return true
	}
	if hosts[q.ReplyToRunID] {
		return c.srv.ExecutionRuntime.ExecutionLaneReady(q.TenantID, q.PersonID, q.ReplyToRunID)
	}
	return c.srv.ExecutionRuntime.ExecutionLaneReady(q.TenantID, q.PersonID)
}
func (d *Server) refreshExecutionWaits(ctx context.Context, tenantID, personID string) (map[string]bool, error) {
	if d == nil || d.Control == nil {
		return map[string]bool{}, nil
	}
	hosts, err := d.Control.QueuedHostExecutionWaits(ctx, tenantID, personID)
	if err != nil {
		return nil, err
	}
	d.ExecutionRuntime.ReconcileQueuedHostExecution(tenantID, personID, hosts)
	refs := make(map[string]bool, len(hosts))
	for _, id := range hosts {
		refs[id] = true
	}
	return refs, nil
}
func (c *RunCoordinator) withExecutionWaitResume(ctx context.Context, identity *control.IdentityContext, parent *control.Run, key string) (context.Context, error) {
	if parent == nil || key != "execution-lane:"+parent.ID || parent.PersonID != identity.PersonID {
		return ctx, fmt.Errorf("execution wait does not name its exact owned parent")
	}
	checkpoint, err := c.srv.Control.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, parent.ID)
	if err != nil {
		return ctx, err
	}
	if checkpoint == nil || checkpoint.Outcome != "await_execution" || (checkpoint.WaitKind != control.LoopCheckpointWaitExecutionLane && checkpoint.WaitKind != control.LoopCheckpointWaitExecutionHostLane) || checkpoint.WaitRef != parent.ID || checkpoint.WaitToolCallID == "" || !kernel.HasLoopResumeMessages(ctx) {
		return ctx, fmt.Errorf("execution wait has no exact pending tool checkpoint; work was not replayed")
	}
	return kernel.WithExecutionWaitResume(ctx, checkpoint.WaitToolCallID), nil
}
