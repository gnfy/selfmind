package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

var errGatewayShutdown = errors.New("gateway shutdown")

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

// stopAllActive interrupts every active run during gateway shutdown. This is
// infrastructure recovery, not a user cancellation: work remains resumable,
// and a drained queue row is reopened so the next daemon can continue it.
func (c *RunCoordinator) stopAllActive(reason string) {
	c.mu.Lock()
	var runs []*activeRun
	for _, active := range c.active {
		copy := *active
		runs = append(runs, &copy)
	}
	c.mu.Unlock()

	store := c.srv.Control
	for _, active := range runs {
		if active.QueueID != "" && store != nil {
			_, _ = store.MarkQueuedIfStatus(context.Background(), active.TenantID, active.QueueID, control.QueueStatusStarted, control.QueueStatusQueued)
		}
		if active.RunID != "" && store != nil {
			_ = store.FinishRun(context.Background(), active.TenantID, active.RunID, "interrupted")
		}
		if active.TaskID != "" && store != nil {
			next := []string{"Reply \"continue\" to resume from the last durable evidence."}
			_ = store.UpdateTaskStatus(context.Background(), active.TenantID, active.TaskID, "interrupted", "Interrupted by gateway shutdown.", next)
			_, _ = store.AppendEvent(context.Background(), gatewayShutdownEvent(active, reason))
		}
		if active.Interrupt != nil {
			active.Interrupt(errGatewayShutdown)
		} else if active.Cancel != nil {
			active.Cancel()
		}
	}
}

func gatewayShutdownEvent(active *activeRun, reason string) control.Event {
	return control.Event{
		TaskID:         active.TaskID,
		RunID:          active.RunID,
		Type:           "run.interrupted",
		Visibility:     "task",
		Channel:        active.Channel,
		Payload:        mustJSON(map[string]string{"reason": reason, "completion_reason": "daemon_recovery"}),
		IdempotencyKey: "run:" + active.RunID + ":gateway-shutdown",
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
	// A drained queue row transitions to 'done' the moment its async run returns
	// through ANY terminal path — normal completion, an early error return, or a
	// panic unwinding through this defer. Marking it done (not leaving it
	// 'started') is what stops boot recovery from re-running already-completed
	// work. Uses a fresh context so a cancelled turn ctx cannot skip the write.
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
	attach.effectKey = strings.TrimSpace(req.EffectKey)
	// Keep the per-person current_task pointer on the task this run actually
	// attached to. Channel-scoped resolution (and async sends in particular)
	// can pick a task that differs from the pointer; without this sync,
	// /status on every endpoint keeps reporting an unrelated old task.
	c.syncCurrentTask(ctx, identity, task)
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "user", req.Content)

	run, err := d.Control.StartRunWithOptions(ctx, task, req.Channel, truncate(req.Content, 240), control.StartRunOptions{
		WorkKey:               attach.workKey,
		PreserveTaskLifecycle: attach.preLabel && !attach.created,
	})
	if err != nil {
		if attach.created {
			_, _ = d.Control.DeleteEmptyTask(context.WithoutCancel(ctx), identity.TenantID, identity.PersonID, task.ID)
		}
		return api.MessageResponse{Identity: identity, Task: task, Error: err.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	if req.QueueID != "" {
		var bindErr error
		if req.QueueClaimToken != "" {
			bindErr = d.Control.BindQueuedRunClaimed(ctx, identity.TenantID, req.QueueID, run.ID, req.QueueClaimToken)
		} else {
			bindErr = d.Control.BindQueuedRun(ctx, identity.TenantID, req.QueueID, run.ID)
		}
		if bindErr != nil {
			summary := "The queued run could not be bound to its durable queue record."
			outcome := api.RunOutcome{
				Status: "failed", Summary: summary,
				NextSteps: []string{"Retry after checking the durable queue state."},
			}
			_ = c.materializeRunFinalization(context.WithoutCancel(ctx), identity, task, run,
				"interrupted", run.WorkspaceID, req.Content, req.Channel, "", outcome, attach,
				control.Handoff{
					TaskID: task.ID, Summary: summary, NextSteps: outcome.NextSteps,
				},
				control.Event{
					TaskID: task.ID, RunID: run.ID, Type: "run.failed", Visibility: "task", Channel: req.Channel,
					Payload: mustJSON(map[string]string{"error": bindErr.Error()}),
				})
			_, _ = d.Control.MarkQueuedIfStatus(context.WithoutCancel(ctx), identity.TenantID, req.QueueID, control.QueueStatusStarted, control.QueueStatusFailed)
			return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: bindErr.Error(), Turn: messageTurn("failed", "interrupted", "idle", task.ID, run.ID, bindErr.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
		}
	}
	stopHeartbeat := c.startRunHeartbeat(ctx, run, req.QueueID, req.QueueClaimToken)
	defer stopHeartbeat()
	c.updateActive(identity.PersonID, task, run)
	startedPayload := map[string]string{"input": truncate(req.Content, 500)}
	if watchID := strings.TrimSpace(req.WatchID); watchID != "" {
		startedPayload["watch_id"] = watchID
		startedPayload["task_status"] = "running"
	}
	if origin := runOrigin(ctx, req); origin != "" {
		startedPayload["origin"] = origin
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.started",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(startedPayload),
	})
	if attach.workKey != "" {
		d.appendLabelAssignedEvent(ctx, task.ID, run.ID, map[string]interface{}{
			"decision": "ingress_work_key",
			"work_key": attach.workKey,
			"task_id":  task.ID,
			"run_id":   run.ID,
		})
	}

	workspace, _ := c.workspaceForTask(ctx, identity, task, req, attach)
	analysisWorkspaceID := run.WorkspaceID
	if workspace != nil && workspace.ID != "" {
		analysisWorkspaceID = workspace.ID
	}
	replay := runMaintenanceReplay{WorkspaceID: analysisWorkspaceID, UserInput: req.Content, Attach: attach}
	if workspace != nil && strings.TrimSpace(workspace.LocalPath) != "" {
		if _, statErr := os.Stat(workspace.LocalPath); statErr != nil {
			summary := fmt.Sprintf("The workspace environment is unavailable: %s", workspace.LocalPath)
			outcome := api.RunOutcome{
				Status:           "waiting_user",
				CompletionReason: "environment_unavailable",
				Resumable:        true,
				Summary:          summary,
				NextSteps:        []string{"Restore or mount the workspace, then reply \"continue\"."},
				Risks:            []string{tools.RedactSensitive(statErr.Error())},
			}
			event := control.Event{
				TaskID: task.ID, RunID: run.ID, Type: "run.waiting_user", Visibility: "task", Channel: req.Channel,
				Payload: mustJSON(map[string]interface{}{"outcome": outcome, "reason": "environment_unavailable"}),
			}
			_ = c.materializeRunFinalization(context.WithoutCancel(ctx), identity, task, run, "waiting_user",
				analysisWorkspaceID, req.Content, req.Channel, "", outcome, attach,
				control.Handoff{TaskID: task.ID, Summary: summary, NextSteps: outcome.NextSteps, Risks: outcome.Risks},
				event)
			return api.MessageResponse{
				Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: summary,
				Turn:    messageTurn("waiting_user", "waiting_user", "idle", task.ID, run.ID, summary),
				Context: messageContextBudget(llmUsageZero()),
			}, http.StatusOK
		}
	}
	lease, leaseErr := c.materializeExecutionLease(ctx, identity, run, workspace)
	if leaseErr != nil {
		// A recovered run whose environment no longer matches what it started
		// with is a lifecycle decision, not a failure: continuing under a
		// different PATH, account, or credential source would silently change
		// what the remaining steps do. Park it for a human like an unavailable
		// workspace, on the same resume path.
		var changed *executionenv.EnvironmentChangedError
		if errors.As(leaseErr, &changed) {
			summary := "The execution environment changed since this run started (" +
				strings.Join(changed.Changed, ", ") + ")."
			outcome := api.RunOutcome{
				Status:           "waiting_user",
				CompletionReason: "environment_changed",
				Resumable:        true,
				Summary:          summary,
				NextSteps: []string{
					"Confirm the intended account and toolchain, then reply \"continue\" to start a fresh run under the current environment.",
				},
			}
			event := control.Event{
				TaskID: task.ID, RunID: run.ID, Type: "run.waiting_user", Visibility: "task", Channel: req.Channel,
				Payload: mustJSON(map[string]interface{}{"outcome": outcome, "reason": "environment_changed", "changed": changed.Changed}),
			}
			_ = c.materializeRunFinalization(context.WithoutCancel(ctx), identity, task, run, "waiting_user",
				analysisWorkspaceID, req.Content, req.Channel, "", outcome, attach,
				control.Handoff{TaskID: task.ID, Summary: summary, NextSteps: outcome.NextSteps},
				event)
			return api.MessageResponse{
				Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: summary,
				Turn:    messageTurn("waiting_user", "waiting_user", "idle", task.ID, run.ID, summary),
				Context: messageContextBudget(llmUsageZero()),
			}, http.StatusOK
		}
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, fmt.Errorf("materialize execution lease: %w", leaseErr), replay)
		return api.MessageResponse{
			Identity: identity, Task: task, Run: run, Outcome: &outcome, Error: firstString(outcome.Risks),
			Turn:    messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary),
			Context: messageContextBudget(llmUsageZero()),
		}, http.StatusOK
	}
	// Import attachment files into the daemon-managed person partition BEFORE
	// scope install and context assembly: the rendered attachment paths and
	// the scope's allowed roots must both point at the managed copies.
	req.Attachments = c.importAttachments(identity, run, req.Attachments)
	cleanupScope := c.installExecutionScope(ctx, identity, task, run, workspace, req, lease)
	defer cleanupScope()
	// Tag the turn with its run-scoped key so tool calls resolve exactly this
	// run's scope instead of the person's most recent one.
	if run != nil {
		ctx = tools.WithExecutionScopeKey(ctx, tools.ExecutionScopeKeyForRun(run.ID))
	}
	if workspace != nil && workspace.LocalPath != "" {
		ctx = kernel.WithWorkspaceContext(ctx, kernel.WorkspaceContext{
			ID:   workspace.ID,
			Root: workspace.LocalPath,
		})
	}
	if sink := c.newToolArtifactSink(identity, task, run); sink != nil {
		ctx = kernel.WithToolArtifactSink(ctx, sink)
	}
	if ledger := c.newToolLedger(identity); ledger != nil {
		ctx = kernel.WithToolLedger(ctx, ledger)
	}
	if sink := c.newLoopCheckpointSink(identity, task, run); sink != nil {
		ctx = kernel.WithLoopCheckpointSink(ctx, sink)
	}
	ctx = kernel.WithTaskRuntimeContext(ctx, c.selectedTaskRuntimeContext(ctx, task, run, workspace, req.Platform, req.Channel, req.Content, attach.preLabel))
	ctx = c.withLoopCheckpointResume(ctx, identity, task, run, intent)
	agentInput := c.withGatewayContext(req.Content, identity, task, workspace, req.Attachments)
	agentInput = c.withResumeContext(ctx, identity, task, run, intent, attach.claimsPriorRuns(), attach.workKey, agentInput)
	// Independent of continuation intent: any run on a task with uncertain
	// (crash-orphaned) side-effect tool calls must verify before repeating
	// (P0-B closure — a boot-requeued run re-drains as a "new" message).
	agentInput = c.withUncertainToolWarning(ctx, identity, task, agentInput)
	ctx = kernel.WithTaskStrategy(ctx, taskStrategyForRequest(req, intent))

	if d.Gateway == nil {
		err := fmt.Errorf("gateway is not configured")
		outcome := api.RunOutcome{Status: "failed", CompletionReason: "gateway_unavailable", Summary: err.Error(), Risks: []string{err.Error()}}
		_ = c.materializeRunFinalization(context.Background(), identity, task, run, "failed", replay.WorkspaceID, replay.UserInput, req.Channel, "", outcome, replay.Attach,
			control.Handoff{TaskID: task.ID, Summary: outcome.Summary, Risks: outcome.Risks},
			control.Event{Type: "run.failed", Visibility: "task", Channel: req.Channel, Payload: mustJSON(map[string]interface{}{"outcome": outcome, "error": err.Error()})})
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn("failed", "failed", "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}

	resp, err := d.Gateway.RunAgentWithEvents(ctx, identity.PersonID, req.Channel, agentInput)
	if err != nil {
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, err, replay)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Error: firstString(outcome.Risks), Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
	}

	content, usage, eventSummary, err := c.aggregateGatewayResponse(ctx, req.Channel, task, run, resp)
	if err != nil {
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, err, replay)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage, Error: firstString(outcome.Risks), Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: messageContextBudget(usage)}, http.StatusOK
	}

	// Finalization must survive turn cancellation. The turn ctx can be
	// cancelled or deadline-expired in the window between the agent stream
	// completing and these writes (observed with eval turn budgets); using the
	// live ctx here made FinishRun/UpdateTaskStatus fail silently and left runs
	// stuck in `running` forever. WithoutCancel keeps ctx values (observers,
	// scopes) while guaranteeing the terminal state lands in control.db.
	finCtx := context.WithoutCancel(ctx)
	outcome := buildRunOutcome(content)
	structuredOutcome := false
	if structured, ok := c.latestStructuredRunOutcome(finCtx, task.ID, run.ID); ok {
		structuredOutcome = true
		outcome = structured
		if outcome.Summary == "" {
			outcome.Summary = buildRunOutcome(content).Summary
		}
	}
	outcome = reconcileTurnCompletion(outcome, eventSummary.Completion(), structuredOutcome)
	verification, evidenceFiles := c.evidenceOutcome(finCtx, task.ID, run.ID)
	outcome.Verification, outcome.Files = mergeEvidenceFiles(verification, evidenceFiles, outcome.Files)
	outcome.ClaimMismatches = verificationClaimMismatches(outcome)
	outcome = applyVerificationOutcome(outcome)
	for _, mismatch := range outcome.ClaimMismatches {
		outcome.Risks = appendUnique(outcome.Risks, mismatch, 8)
	}
	content = withVerificationNotice(content, outcome.Verification, outcome.ClaimMismatches)
	var finalizeErrs []string
	recordFinalizeErr := func(action string, err error) {
		if err == nil {
			return
		}
		msg := fmt.Sprintf("%s: %v", action, err)
		finalizeErrs = append(finalizeErrs, msg)
		log.Error("run finalization failed", "action", action, "task_id", task.ID, "run_id", run.ID, "error", err)
	}

	// Invariant: finalization must leave the run terminal and must never leave
	// the task 'running' with zero live runs. A 'running' outcome means "turn
	// finished, more work planned", so the task parks as 'in_progress' — still
	// non-terminal (resolveContinueTask keeps offering it for `继续`/`/resume`)
	// but honest in /tasks and /status: nothing is executing anymore.
	// Store.FinishRun coerces the run-side status to a terminal value itself.
	// Run completion and task-label lifecycle are different contracts. A plain
	// answer completes this run, but it must not close the person's reusable
	// work label. Only an explicit structured outcome may terminalize the task.
	taskStatus := taskStatusForFinalization(outcome, structuredOutcome)
	handoff := control.Handoff{
		TaskID:       task.ID,
		Summary:      outcome.Summary,
		DoneItems:    outcome.Done,
		NextSteps:    outcome.NextSteps,
		ChangedFiles: outcome.Files,
		TestStatus:   strings.Join(outcome.Tests, "\n"),
		Risks:        outcome.Risks,
	}
	terminalEvent := control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.finished",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]interface{}{"outcome": outcome, "usage": usage}),
	}
	recordFinalizeErr("materialize run finalization", c.materializeRunFinalization(finCtx, identity, task, run, taskStatus,
		replay.WorkspaceID, replay.UserInput, req.Channel, content, outcome, replay.Attach, handoff, terminalEvent))
	c.recordOutcomeArtifacts(finCtx, task, run, req.Channel, outcome.Files)
	if len(finalizeErrs) > 0 {
		_, _ = d.Control.AppendEvent(context.Background(), control.Event{
			TaskID:     task.ID,
			RunID:      run.ID,
			Type:       "run.finalize_error",
			Visibility: "task",
			Channel:    req.Channel,
			Payload:    mustJSON(map[string]interface{}{"errors": finalizeErrs}),
		})
	}
	taskID := task.ID
	refreshed, _ := d.Control.GetTask(finCtx, identity.TenantID, task.ID)
	if refreshed != nil && refreshed.Status != "" {
		taskStatus = refreshed.Status
	}
	if refreshed == nil {
		refreshed = task
	}
	turnStatus := turnStatusForOutcome(outcome)
	out := api.MessageResponse{Identity: identity, Task: refreshed, Run: run, Outcome: &outcome, Content: content, Usage: usage, Turn: messageTurn(turnStatus, taskStatus, "idle", taskID, run.ID, outcome.Summary), Context: messageContextBudget(usage)}
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

// Origins of a run the daemon started on the person's behalf. A turn the
// person typed at any endpoint has no origin: it is their own foreground work,
// wherever they typed it.
const (
	runOriginWatch = "watch"
	runOriginCron  = "cron"
)

// runOrigin names the initiator of a daemon-started run, or "" for a person's
// own turn. Explicit request state wins; a watcher finalization is inferred
// from the watch id the queue drain derives from its durable key; the kernel
// turn-source tag is the last resort, because it only survives synchronous
// paths — an async run executes under a fresh context.Background().
func runOrigin(ctx context.Context, req api.MessageRequest) string {
	if origin := strings.TrimSpace(req.Origin); origin != "" {
		return origin
	}
	if strings.TrimSpace(req.WatchID) != "" {
		return runOriginWatch
	}
	return strings.TrimSpace(kernel.TurnSourceFromContext(ctx))
}

// startAsyncRun accepts a turn immediately and runs it in the background,
// delivering the result back to the source channel when it finishes.
func (c *RunCoordinator) startAsyncRun(identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) api.MessageResponse {
	active := &activeRun{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Channel:        req.Channel,
		Platform:       req.Platform,
		PlatformUserID: req.PlatformUserID,
		WorkspaceID:    req.WorkspaceID,
		ApprovalMode:   req.ApprovalMode,
		QueueID:        req.QueueID,
		Summary:        truncate(req.Content, 240),
		StartedAt:      time.Now(),
		// Registered here so /v1/runs/steer can inject guidance; wired into the
		// run ctx below so the agent loop drains it at iteration boundaries.
		Steer: make(chan kernel.SteeringInput, steerBufferSize),
	}
	if ok := c.beginActive(identity.PersonID, active); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop.", Turn: messageTurn("busy", "running", "running", "", "", "")}
	}

	runCtx, cancelCause := context.WithCancelCause(context.Background())
	runCancel := func() { cancelCause(context.Canceled) }
	runCtx = kernel.WithSteeringInputs(runCtx, active.Steer)
	active.Cancel = runCancel
	active.Interrupt = cancelCause
	stopProgressNotices := c.startAsyncProgressNotices(runCtx, identity, req)
	go func() {
		// Defers run LIFO: drainQueue is registered first so it runs LAST —
		// after endActive frees the per-person slot — chaining the next queued
		// item into its own async run once this one is truly done.
		defer c.drainQueue(identity)
		// Unconsumed steering outlives the run as durable queued work pinned
		// to the task (P0-A). Runs BEFORE drainQueue (LIFO) so the deferral is
		// visible to that very drain.
		defer c.deferUnconsumedSteering(identity, active)
		defer c.endActive(identity.PersonID)
		defer runCancel()
		defer stopProgressNotices()
		// Panic firewall: async runs (IM, queue drain, cron, detached CLI) have
		// no net/http per-request recover shielding them, so an unrecovered panic
		// in runMessage would crash the ENTIRE gateway daemon. Registered LAST so
		// it unwinds FIRST — before endActive/drainQueue — letting it read the
		// active-run registry to finalize the run/task while the slot still
		// exists. endActive + drainQueue then still run, so the person is never
		// left wedged behind a dead run.
		defer func() {
			if r := recover(); r != nil {
				accepted := c.recoverAsyncRun(identity, req, r)
				c.settleAsyncQueue(identity, req, accepted)
			}
		}()
		resp, _ := c.runMessage(runCtx, identity, req, intent)
		accepted := c.deliverAsyncResult(context.Background(), identity, req, resp)
		c.settleAsyncQueue(identity, req, accepted)
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

// recoverAsyncRun contains a panicked async run so the daemon survives. It logs
// the panic with its stack, then reuses the ordinary failure-finalize contract:
// the run is marked failed and the task interrupted (non-terminal/resumable), so
// status, the active-run registry, and the queue stay consistent — the caller's
// deferred endActive + drainQueue still run afterward, freeing the person's slot
// so they are not wedged. It never re-panics. Run/task ids come from the active
// registry snapshot (still present because this defer unwinds before endActive).
func (c *RunCoordinator) recoverAsyncRun(identity *control.IdentityContext, req api.MessageRequest, r interface{}) bool {
	log.Error("async run panicked; recovered to keep the gateway alive",
		"person", identity.PersonID, "channel", req.Channel,
		"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
	if c.srv == nil || c.srv.Control == nil {
		return false
	}
	active := c.currentActive(identity.PersonID)
	if active == nil {
		// Panic before the run was registered (e.g. during workspace/task
		// resolution): nothing to finalize; endActive/drainQueue handle the slot.
		return false
	}
	ctx := context.Background()
	tenant := active.TenantID
	if tenant == "" {
		tenant = identity.TenantID
	}
	outcome := api.RunOutcome{
		Status:           "interrupted",
		CompletionReason: "internal_error",
		Resumable:        true,
		Summary:          "The run was interrupted by an internal error.",
		NextSteps:        []string{"Reply \"continue\" to resume from the last durable evidence."},
		Risks:            []string{"The previous run ended unexpectedly before it could report a complete result."},
	}
	materialized := false
	if active.RunID != "" {
		// Reconstruct the normal atomic finalizer so a panic cannot leave a
		// terminal run without the durable post-run maintenance evidence used by
		// task labels and memory governance. Fall back to the administrative
		// finalizer only when the rows themselves cannot be recovered.
		task, taskErr := c.srv.Control.GetTask(ctx, tenant, active.TaskID)
		run, runErr := c.srv.Control.GetRun(ctx, tenant, active.RunID)
		if taskErr == nil && runErr == nil && task != nil && run != nil {
			event := control.Event{
				TaskID: task.ID, RunID: run.ID, Type: "run.failed", Visibility: "task", Channel: active.Channel,
				Payload: mustJSON(map[string]string{"error": "internal error", "reason": "run panicked and was recovered"}),
			}
			handoff := control.Handoff{
				TaskID: task.ID, Summary: outcome.Summary, NextSteps: outcome.NextSteps, Risks: outcome.Risks,
			}
			if err := c.materializeRunFinalization(ctx, identity, task, run, "interrupted", run.WorkspaceID, req.Content, active.Channel, "", outcome, taskAttach{}, handoff, event); err != nil {
				log.Warn("panic recovery: atomic run finalization failed", "run", active.RunID, "error", err)
				_ = c.srv.Control.FinishRun(ctx, tenant, active.RunID, "failed")
			} else {
				materialized = true
			}
		} else {
			log.Warn("panic recovery: run context unavailable; using fallback finalizer",
				"run", active.RunID, "task_error", taskErr, "run_error", runErr)
			_ = c.srv.Control.FinishRun(ctx, tenant, active.RunID, "failed")
		}
	}
	if active.TaskID != "" && !materialized {
		_ = c.srv.Control.UpdateTaskStatus(ctx, tenant, active.TaskID, "interrupted", "run aborted by internal error", nil)
		_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
			TaskID:     active.TaskID,
			RunID:      active.RunID,
			Type:       "run.failed",
			Visibility: "task",
			Channel:    active.Channel,
			Payload:    mustJSON(map[string]string{"error": "internal error", "reason": "run panicked and was recovered"}),
		})
	}
	// Let the origin endpoint know the turn ended instead of hanging forever.
	return c.deliverAsyncResult(ctx, identity, req, api.MessageResponse{
		Identity: identity,
		Outcome:  &outcome,
		Error:    "internal error: the run was aborted",
		Turn:     messageTurn("failed", "interrupted", "idle", active.TaskID, active.RunID, "run aborted by internal error"),
	})
}

func (c *RunCoordinator) settleAsyncQueue(identity *control.IdentityContext, req api.MessageRequest, deliveryAccepted bool) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || req.QueueID == "" {
		return
	}
	if req.EffectKey != "" && !deliveryAccepted {
		return
	}
	_, _ = c.srv.Control.MarkQueuedIfStatus(context.Background(), identity.TenantID, req.QueueID, control.QueueStatusStarted, control.QueueStatusDone)
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
	var claimToken string
	for {
		var err error
		next, err = c.srv.Control.NextQueued(ctx, identity.TenantID, personID)
		if err != nil || next == nil {
			return
		}
		var claimed bool
		claimToken, claimed, err = c.srv.Control.ClaimQueued(ctx, identity.TenantID, next.ID, 0)
		if err != nil || !claimed {
			return
		}
		// A queued row is re-validated at drain time with today's inbound
		// rules: command-shaped content no control command claims is a
		// mistyped COMMAND, not work — cancel it instead of launching an agent
		// run. This also flushes poison rows enqueued before the unknown-slash
		// reject gate existed (observed live: a queued "/qwer" resurrected as
		// an agent task at every boot). A "/"-leading path stays queued work
		// (command.LooksLikeCommand). Loop on to the next real item.
		if trimmed := strings.TrimSpace(next.Content); command.LooksLikeCommand(trimmed) {
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
		// A system-originated finalization row carries the task it closes;
		// resolveTask honors an explicit TaskID before any label guess.
		TaskID: next.TaskID,
		Async:  true,
		// Carry the queue row id so the drained run's finalization marks it done
		// (QueueStatusDone) — otherwise the row stays 'started' and boot recovery
		// re-runs the already-completed work.
		QueueID:         next.ID,
		QueueClaimToken: claimToken,
		EffectKey:       next.IdempotencyKey,
	}
	if strings.HasPrefix(next.IdempotencyKey, "external-watch:") {
		req.ExecutionProfile = tools.ExecutionProfileWatchFinalization
		req.WatchID = externalWatchIDFromFinalizationKey(next.IdempotencyKey)
		req.Origin = runOriginWatch
	}
	// Reproduce the queued item's route while preserving its durable person.
	// Never create an account here: system rows may intentionally omit
	// platform_user_id, and normalizing that blank value to cli:local used to
	// move external-watch finalization onto a different person.
	drainIdentity := c.srv.routeIdentityForPerson(ctx, next.TenantID, next.PersonID, next.Channel, next.Platform, identity)
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
