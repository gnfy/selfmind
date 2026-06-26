package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
)

// RunCoordinator owns agent run execution and the per-person active-run
// registry. It is the seam between the gateway's thin HTTP/orchestration layer
// (Server) and the run lifecycle: Server resolves identity, control commands,
// and intent, then hands accepted turns to the coordinator.
//
// The coordinator holds a back-reference to Server for the shared,
// pre/post-run helpers (workspace/task resolution, context assembly, stream
// aggregation, outcome persistence) and reads live dependencies (Control,
// Gateway, Delivery) from it — Delivery is assigned after Server construction,
// so caching it here would be stale. Run-lifecycle state (the active map) lives
// here under the coordinator's own mutex, independent of Server's draining
// lock. Extracting the remaining leaf helpers onto the coordinator is a
// follow-up tracked in docs/STATUS.md.
type RunCoordinator struct {
	srv *Server

	mu     sync.Mutex
	active map[string]*activeRun
}

// coordinator lazily builds the per-Server RunCoordinator. Lazy construction
// keeps every Server entry point (HTTP handlers, IM adapters, eval harness)
// working regardless of how the Server struct was assembled.
func (d *Server) coordinator() *RunCoordinator {
	d.runsOnce.Do(func() {
		d.runs = &RunCoordinator{srv: d, active: map[string]*activeRun{}}
	})
	return d.runs
}

func (c *RunCoordinator) beginActive(personID string, run *activeRun) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = make(map[string]*activeRun)
	}
	if _, exists := c.active[personID]; exists {
		return false
	}
	c.active[personID] = run
	return true
}

func (c *RunCoordinator) updateActive(personID string, task *control.Task, run *control.Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return
	}
	active := c.active[personID]
	if active == nil {
		return
	}
	if task != nil {
		active.TaskID = task.ID
	}
	if run != nil {
		active.RunID = run.ID
		active.StartedAt = run.StartedAt
	}
}

func (c *RunCoordinator) currentActive(personID string) *activeRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return nil
	}
	active := c.active[personID]
	if active == nil {
		return nil
	}
	copy := *active
	return &copy
}

func (c *RunCoordinator) stopActive(personID string) *activeRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return nil
	}
	active := c.active[personID]
	if active == nil {
		return nil
	}
	if active.Cancel != nil {
		active.Cancel()
	}
	copy := *active
	return &copy
}

func (c *RunCoordinator) endActive(personID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		delete(c.active, personID)
	}
}

// activeRunStatuses returns a snapshot of every active run for the status API
// and drain idle-wait.
func (c *RunCoordinator) activeRunStatuses() []api.ActiveRunStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.active) == 0 {
		return nil
	}
	statuses := make([]api.ActiveRunStatus, 0, len(c.active))
	for _, active := range c.active {
		if active == nil {
			continue
		}
		status := formatActiveRunStatus(activeRunCopy(active))
		if status != nil {
			statuses = append(statuses, *status)
		}
	}
	return statuses
}

// stopAllActive cancels every active run and marks the underlying tasks/runs
// cancelled. Used during gateway drain/shutdown.
func (c *RunCoordinator) stopAllActive(reason string) {
	c.mu.Lock()
	var runs []*activeRun
	for _, active := range c.active {
		copy := *active
		runs = append(runs, &copy)
		if active.Cancel != nil {
			active.Cancel()
		}
	}
	c.mu.Unlock()

	control := c.srv.Control
	for _, active := range runs {
		if active.RunID != "" && control != nil {
			_ = control.RequestRunCancel(context.Background(), active.TenantID, active.RunID)
			_ = control.FinishRun(context.Background(), active.TenantID, active.RunID, "cancelled")
		}
		if active.TaskID != "" && control != nil {
			_ = control.UpdateTaskStatus(context.Background(), active.TenantID, active.TaskID, "cancelled", reason, nil)
			_, _ = control.AppendEvent(context.Background(), controlEvent(active, reason))
		}
	}
}

func controlEvent(active *activeRun, reason string) control.Event {
	return control.Event{
		TaskID:     active.TaskID,
		RunID:      active.RunID,
		Type:       "run.cancelled",
		Visibility: "task",
		Channel:    active.Channel,
		Payload:    mustJSON(map[string]string{"reason": reason}),
	}
}

func activeRunCopy(active *activeRun) *activeRun {
	if active == nil {
		return nil
	}
	copy := *active
	return &copy
}

// runMessage executes one synchronous agent run end to end: resolve workspace
// and task, start the run, install the workspace/approval execution scope,
// assemble runtime context, drive the agent with events, then persist the
// structured outcome, handoff, artifacts, and finishing event.
func (c *RunCoordinator) runMessage(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (api.MessageResponse, int) {
	d := c.srv
	if _, err := d.prepareRequestWorkspace(ctx, identity, &req); err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	task, err := d.resolveTask(ctx, identity, req, intent)
	if err != nil {
		if strings.Contains(err.Error(), "no task to continue") {
			return api.MessageResponse{Identity: identity, Content: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
		}
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "user", req.Content)

	run, err := d.Control.StartRun(ctx, task, req.Channel, truncate(req.Content, 240))
	if err != nil {
		return api.MessageResponse{Identity: identity, Task: task, Error: err.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	stopHeartbeat := d.startRunHeartbeat(ctx, run)
	defer stopHeartbeat()
	c.updateActive(identity.PersonID, task, run)
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.started",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]string{"input": truncate(req.Content, 500)}),
	})

	workspace, _ := d.workspaceForTask(ctx, identity, task, req)
	cleanupScope := d.installExecutionScope(identity, task, run, workspace)
	defer cleanupScope()
	if workspace != nil && workspace.LocalPath != "" {
		ctx = kernel.WithWorkspaceContext(ctx, kernel.WorkspaceContext{
			ID:   workspace.ID,
			Root: workspace.LocalPath,
		})
	}
	ctx = kernel.WithTaskRuntimeContext(ctx, d.selectedTaskRuntimeContext(ctx, task, run, workspace, req.Channel))
	agentInput := d.withGatewayContext(req.Content, identity, task, workspace, req.Attachments)
	agentInput = d.withResumeContext(ctx, identity, task, run, intent, agentInput)
	ctx = kernel.WithTaskStrategy(ctx, taskStrategyForRequest(req, intent))

	if d.Gateway == nil {
		err := fmt.Errorf("gateway is not configured")
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, "failed")
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, "blocked", err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn("failed", "blocked", "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}

	resp, err := d.Gateway.RunAgentWithEvents(ctx, identity.PersonID, req.Channel, agentInput)
	if err != nil {
		status := "failed"
		taskStatus := "blocked"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn(status, taskStatus, "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
	}

	content, usage, err := d.aggregateGatewayResponse(ctx, req.Channel, task, run, resp)
	if err != nil {
		status := "failed"
		taskStatus := "blocked"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn(status, taskStatus, "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(usage)}, http.StatusOK
	}

	outcome := buildRunOutcome(content)
	if structured, ok := d.latestStructuredRunOutcome(ctx, task.ID, run.ID); ok {
		outcome = structured
		if outcome.Summary == "" {
			outcome.Summary = buildRunOutcome(content).Summary
		}
	}
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "assistant", content)
	_ = d.Control.FinishRun(ctx, identity.TenantID, run.ID, outcome.Status)
	_ = d.Control.UpdateTaskStatus(ctx, identity.TenantID, task.ID, outcome.Status, outcome.Summary, outcome.NextSteps)
	_, _ = d.Control.SaveHandoff(ctx, control.Handoff{
		TaskID:       task.ID,
		Summary:      outcome.Summary,
		DoneItems:    outcome.Done,
		NextSteps:    outcome.NextSteps,
		ChangedFiles: outcome.Files,
		TestStatus:   strings.Join(outcome.Tests, "\n"),
		Risks:        outcome.Risks,
	})
	d.recordOutcomeArtifacts(ctx, task, run, req.Channel, outcome.Files)
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.finished",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]interface{}{"outcome": outcome, "usage": usage}),
	})
	taskID := task.ID
	task, _ = d.Control.GetTask(ctx, identity.TenantID, task.ID)
	taskStatus := outcome.Status
	if task != nil && task.Status != "" {
		taskStatus = task.Status
	}
	return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage, Turn: messageTurn("completed", taskStatus, "idle", taskID, run.ID, outcome.Summary), Context: messageContextBudget(usage)}, http.StatusOK
}

// startAsyncRun accepts a turn immediately and runs it in the background,
// delivering the result back to the source channel when it finishes.
func (c *RunCoordinator) startAsyncRun(identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) api.MessageResponse {
	d := c.srv
	active := &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		Channel:   req.Channel,
		Summary:   truncate(req.Content, 240),
		StartedAt: time.Now(),
	}
	if ok := c.beginActive(identity.PersonID, active); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop.", Turn: messageTurn("busy", "running", "running", "", "", "")}
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	active.Cancel = runCancel
	stopProgressNotices := d.startAsyncProgressNotices(runCtx, identity, req)
	go func() {
		defer c.endActive(identity.PersonID)
		defer runCancel()
		defer stopProgressNotices()
		resp, _ := c.runMessage(runCtx, identity, req, intent)
		d.deliverAsyncResult(context.Background(), identity, req, resp)
	}()

	notice := router.WorkingNotice(req.Channel)
	if notice == "" {
		notice = "Started in the background. Use /status to check progress or /stop to cancel."
	}
	return api.MessageResponse{
		Identity: identity,
		Content:  notice,
		Accepted: true,
		Turn:     messageTurn("accepted", "running", "running", "", "", notice),
	}
}
