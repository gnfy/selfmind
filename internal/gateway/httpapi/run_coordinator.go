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
	"selfmind/internal/platform/log"
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
	// draining guards the per-person queue drain against re-entrancy: a run
	// finalization triggers a drain, which launches the next queued item as an
	// async run, whose OWN finalization drains again — a chain that must never
	// run two drains for one person concurrently. Keyed by person_id.
	draining map[string]bool
}

// coordinator lazily builds the per-Server RunCoordinator. Lazy construction
// keeps every Server entry point (HTTP handlers, IM adapters, eval harness)
// working regardless of how the Server struct was assembled.
func (d *Server) coordinator() *RunCoordinator {
	d.runsOnce.Do(func() {
		d.runs = &RunCoordinator{srv: d, active: map[string]*activeRun{}, draining: map[string]bool{}}
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
	if _, err := c.prepareRequestWorkspace(ctx, identity, &req); err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	task, attach, err := c.resolveTask(ctx, identity, req, intent)
	if err != nil {
		if strings.Contains(err.Error(), "no task to continue") {
			return api.MessageResponse{Identity: identity, Content: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
		}
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	// Keep the per-person current_task pointer on the task this run actually
	// attached to. Channel-scoped resolution (and async sends in particular)
	// can pick a task that differs from the pointer; without this sync,
	// /status on every endpoint keeps reporting an unrelated old task.
	c.syncCurrentTask(ctx, identity, task)
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "user", req.Content)

	run, err := d.Control.StartRun(ctx, task, req.Channel, truncate(req.Content, 240))
	if err != nil {
		return api.MessageResponse{Identity: identity, Task: task, Error: err.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	stopHeartbeat := c.startRunHeartbeat(ctx, run)
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

	workspace, _ := c.workspaceForTask(ctx, identity, task, req, attach)
	cleanupScope := c.installExecutionScope(identity, task, run, workspace, req)
	defer cleanupScope()
	if workspace != nil && workspace.LocalPath != "" {
		ctx = kernel.WithWorkspaceContext(ctx, kernel.WorkspaceContext{
			ID:   workspace.ID,
			Root: workspace.LocalPath,
		})
	}
	ctx = kernel.WithTaskRuntimeContext(ctx, c.selectedTaskRuntimeContext(ctx, task, run, workspace, req.Channel, req.Content))
	agentInput := c.withGatewayContext(req.Content, identity, task, workspace, req.Attachments)
	agentInput = c.withResumeContext(ctx, identity, task, run, intent, agentInput)
	ctx = kernel.WithTaskStrategy(ctx, taskStrategyForRequest(req, intent))

	if d.Gateway == nil {
		err := fmt.Errorf("gateway is not configured")
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, "failed")
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, "failed", err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn("failed", "failed", "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}

	resp, err := d.Gateway.RunAgentWithEvents(ctx, identity.PersonID, req.Channel, agentInput)
	if err != nil {
		status := "failed"
		// A run error (provider transport failure, model error) is NOT
		// "blocked" — blocked means waiting on the USER (approval/question).
		// Park the task interrupted: non-terminal and resumable, so a
		// continuation ("continue"/resume) retries it and /cancel ends it.
		// Observed live: a codex EOF left a task "blocked" with no way to
		// retry or finish.
		taskStatus := "interrupted"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn(status, taskStatus, "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
	}

	content, usage, err := c.aggregateGatewayResponse(ctx, req.Channel, task, run, resp)
	if err != nil {
		status := "failed"
		// A run error (provider transport failure, model error) is NOT
		// "blocked" — blocked means waiting on the USER (approval/question).
		// Park the task interrupted: non-terminal and resumable, so a
		// continuation ("continue"/resume) retries it and /cancel ends it.
		// Observed live: a codex EOF left a task "blocked" with no way to
		// retry or finish.
		taskStatus := "interrupted"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn(status, taskStatus, "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(usage)}, http.StatusOK
	}

	// Finalization must survive turn cancellation. The turn ctx can be
	// cancelled or deadline-expired in the window between the agent stream
	// completing and these writes (observed with eval turn budgets); using the
	// live ctx here made FinishRun/UpdateTaskStatus fail silently and left runs
	// stuck in `running` forever. WithoutCancel keeps ctx values (observers,
	// scopes) while guaranteeing the terminal state lands in control.db.
	finCtx := context.WithoutCancel(ctx)
	outcome := buildRunOutcome(content)
	if structured, ok := c.latestStructuredRunOutcome(finCtx, task.ID, run.ID); ok {
		outcome = structured
		if outcome.Summary == "" {
			outcome.Summary = buildRunOutcome(content).Summary
		}
	}
	_ = d.Control.RecordChannelMessage(finCtx, *identity, req.Channel, task.ID, "assistant", content)
	// Invariant: finalization must leave the run terminal and must never leave
	// the task 'running' with zero live runs. A 'running' outcome means "turn
	// finished, more work planned", so the task parks as 'in_progress' — still
	// non-terminal (resolveContinueTask keeps offering it for `继续`/`/resume`)
	// but honest in /tasks and /status: nothing is executing anymore.
	// Store.FinishRun coerces the run-side status to a terminal value itself.
	taskStatus := outcome.Status
	if taskStatus == "" || taskStatus == "running" {
		taskStatus = "in_progress"
	}
	_ = d.Control.FinishRun(finCtx, identity.TenantID, run.ID, outcome.Status)
	_ = d.Control.UpdateTaskStatus(finCtx, identity.TenantID, task.ID, taskStatus, outcome.Summary, outcome.NextSteps)
	_, _ = d.Control.SaveHandoff(finCtx, control.Handoff{
		TaskID:       task.ID,
		Summary:      outcome.Summary,
		DoneItems:    outcome.Done,
		NextSteps:    outcome.NextSteps,
		ChangedFiles: outcome.Files,
		TestStatus:   strings.Join(outcome.Tests, "\n"),
		Risks:        outcome.Risks,
	})
	c.recordOutcomeArtifacts(finCtx, task, run, req.Channel, outcome.Files)
	_, _ = d.Control.AppendEvent(finCtx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.finished",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]interface{}{"outcome": outcome, "usage": usage}),
	})
	taskID := task.ID
	refreshed, _ := d.Control.GetTask(finCtx, identity.TenantID, task.ID)
	if refreshed != nil && refreshed.Status != "" {
		taskStatus = refreshed.Status
	}
	if refreshed == nil {
		refreshed = task
	}
	out := api.MessageResponse{Identity: identity, Task: refreshed, Run: run, Outcome: &outcome, Content: content, Usage: usage, Turn: messageTurn("completed", taskStatus, "idle", taskID, run.ID, outcome.Summary), Context: messageContextBudget(usage)}
	// Post-run labeler (Work Timeline P3): asynchronously check the pre-label
	// guess against the person's open labels. Runs under the finalize ctx
	// (WithoutCancel) AFTER the response is assembled, never blocks it, and
	// no-ops when nil.
	c.labelFinishedRunAsync(finCtx, identity, task, run, req.Content, outcome.Summary, attach)
	return out, http.StatusOK
}

// syncCurrentTask moves the per-person current_task pointer to the task a run
// resolved, reusing the same SetCurrentTask mechanism as the /new and /resume
// control commands (never a second pointer). Best-effort and write-avoiding:
// it only writes when the pointer actually differs, and a pointer-read error
// falls through to the write so the pointer converges rather than staying
// stale.
func (c *RunCoordinator) syncCurrentTask(ctx context.Context, identity *control.IdentityContext, task *control.Task) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil {
		return
	}
	if current, err := c.srv.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID); err == nil && current != nil && current.ID == task.ID {
		return
	}
	_ = c.srv.Control.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID)
}

// startAsyncRun accepts a turn immediately and runs it in the background,
// delivering the result back to the source channel when it finishes.
func (c *RunCoordinator) startAsyncRun(identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) api.MessageResponse {
	active := &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		Channel:   req.Channel,
		Summary:   truncate(req.Content, 240),
		StartedAt: time.Now(),
		// Registered here so /v1/runs/steer can inject guidance; wired into the
		// run ctx below so the agent loop drains it at iteration boundaries.
		Steer: make(chan string, steerBufferSize),
	}
	if ok := c.beginActive(identity.PersonID, active); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop.", Turn: messageTurn("busy", "running", "running", "", "", "")}
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	runCtx = kernel.WithSteering(runCtx, active.Steer)
	active.Cancel = runCancel
	stopProgressNotices := c.startAsyncProgressNotices(runCtx, identity, req)
	go func() {
		// Defers run LIFO: drainQueue is registered first so it runs LAST —
		// after endActive frees the per-person slot — chaining the next queued
		// item into its own async run once this one is truly done.
		defer c.drainQueue(identity)
		defer c.endActive(identity.PersonID)
		defer runCancel()
		defer stopProgressNotices()
		resp, _ := c.runMessage(runCtx, identity, req, intent)
		c.deliverAsyncResult(context.Background(), identity, req, resp)
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

// drainQueue starts the next queued task for a person as an async run, once no
// run is active. It is the auto-start half of "queue instead of busy" (G1+G2):
// called after every run finalization (sync and async paths) and at boot.
//
// Re-entrancy and races are handled up front: the draining flag serializes
// drains for a person, and the active-run check refuses to launch while a run
// is (still or again) executing. If beginActive races and loses to a fresh
// inbound run, the row is reverted to queued so the NEXT finalization drains it
// — a queued item is never silently dropped.
func (c *RunCoordinator) drainQueue(identity *control.IdentityContext) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
		return
	}
	personID := identity.PersonID
	c.mu.Lock()
	if c.active[personID] != nil { // a run raced in; its own finalize will drain
		c.mu.Unlock()
		return
	}
	if c.draining == nil {
		c.draining = map[string]bool{}
	}
	if c.draining[personID] {
		c.mu.Unlock()
		return
	}
	c.draining[personID] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.draining, personID)
		c.mu.Unlock()
	}()

	ctx := context.Background()
	var next *control.QueuedTask
	for {
		var err error
		next, err = c.srv.Control.NextQueued(ctx, identity.TenantID, personID)
		if err != nil || next == nil {
			return
		}
		if err := c.srv.Control.MarkQueued(ctx, identity.TenantID, next.ID, control.QueueStatusStarted); err != nil {
			return
		}
		// A queued row is re-validated at drain time with today's inbound
		// rules: slash-shaped content no control command claims is a mistyped
		// COMMAND, not work — cancel it instead of launching an agent run.
		// This also flushes poison rows enqueued before the unknown-slash
		// reject gate existed (observed live: a queued "/qwer" resurrected as
		// an agent task at every boot). Loop on to the next real item.
		if trimmed := strings.TrimSpace(next.Content); strings.HasPrefix(trimmed, "/") {
			_ = c.srv.Control.MarkQueued(ctx, identity.TenantID, next.ID, control.QueueStatusCancelled)
			log.Warn("gateway: cancelled queued slash-command row instead of draining it", "content", trimmed)
			continue
		}
		break
	}
	req := api.MessageRequest{
		TenantID:       next.TenantID,
		Platform:       next.Platform,
		PlatformUserID: next.PlatformUserID,
		Channel:        next.Channel,
		Content:        next.Content,
		ApprovalMode:   next.ApprovalMode,
		WorkspaceID:    next.WorkspaceID,
		Async:          true,
	}
	drainIdentity := identity
	// Reproduce the queued item's own account (platform/platform_user_id may
	// differ from the endpoint that just finished, e.g. a CLI run draining a
	// Telegram-queued task) so its result routes back to the right origin.
	if resolved, rerr := c.srv.Control.ResolveOrCreateAccount(ctx, next.TenantID, next.Platform, next.PlatformUserID, ""); rerr == nil && resolved != nil {
		drainIdentity = resolved
	}
	// A drained item is an ordinary agent-bound message: rules-based intent
	// only, and resolveTask gives it the same pre-label guess as any other
	// message (Work Timeline P3) — harmless, since labels never gate context.
	intent := c.srv.classifyIntent(ctx, req.Content, req.Channel)
	resp := c.startAsyncRun(drainIdentity, req, intent)
	if !resp.Accepted {
		// A fresh inbound run won the slot between our check and beginActive.
		// Revert so this item is drained on the next finalization.
		_ = c.srv.Control.MarkQueued(ctx, next.TenantID, next.ID, control.QueueStatusQueued)
	}
}
