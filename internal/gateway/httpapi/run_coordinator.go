package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
	"selfmind/internal/runpool"
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
	active map[string]map[*activeRun]struct{}
	// activeLimit is a process-local guard while a turn is being admitted. The
	// durable Run row remains the execution authority after admission.
	activeLimit int
	// draining guards the per-person queue drain against re-entrancy: a run
	// finalization triggers a drain, which launches the next queued item as an
	// async run, whose OWN finalization drains again — a chain that must never
	// run two drains for one person concurrently. Keyed by person_id.
	draining map[string]bool
	// unreadableQueueReported keeps the warning for a queued row with an
	// unreadable admission snapshot to once per row per process.
	unreadableQueueReported sync.Map
	// cronLookupReported keeps the warning for a queued scheduled fire whose
	// schedule could not be looked up to once per row per process.
	cronLookupReported sync.Map
	// pendingQueueReturns retries only claims whose dispatcher confirmed no
	// Run started. Rows and tokens remain durable; boot recovery owns a crash.
	pendingQueueReturns sync.Map
	// Startup notices whose outbox write failed are retried without needing
	// another work slot or another attempt to execute the queued input.
	pendingQueueNotices sync.Map
	// unreadableNotices holds, per such row, what the person has been told.
	unreadableNotices sync.Map
	// unreadableNoticeRetry is how long a notice whose durable write or push
	// failed waits before it is attempted again; zero is the default.
	unreadableNoticeRetry time.Duration
	// approvalModeRead reads the person's persisted /mode and when it was
	// written. Nil reads the control store; it is a seam for injecting a read
	// that fails.
	approvalModeRead func(ctx context.Context, tenantID, personID string) (string, int64, error)
}

// coordinator lazily builds the per-Server RunCoordinator. Lazy construction
// keeps every Server entry point (HTTP handlers, IM adapters, eval harness)
// working regardless of how the Server struct was assembled.
func (d *Server) coordinator() *RunCoordinator {
	d.runsOnce.Do(func() {
		d.runs = &RunCoordinator{srv: d, active: map[string]map[*activeRun]struct{}{}, activeLimit: 1, draining: map[string]bool{}}
	})
	return d.runs
}

// ConfigureWorkRunCapacity is called before the daemon starts accepting
// requests. The durable admission check and the process registry must use the
// same ceiling, and each simultaneously executing Run needs its own Agent.
func (d *Server) ConfigureWorkRunCapacity(limit, workers int) error {
	if d == nil || limit < 1 || limit > 3 {
		return fmt.Errorf("gateway.max_active_work_runs must be between 1 and 3")
	}
	if limit > workers {
		return fmt.Errorf("gateway.max_active_work_runs (%d) requires at least %d agent workers", limit, limit)
	}
	c := d.coordinator()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.active) != 0 {
		return fmt.Errorf("cannot change work Run capacity while Runs are active")
	}
	c.activeLimit = limit
	return nil
}

func (c *RunCoordinator) beginActive(personID string, run *activeRun) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.beginActiveLocked(personID, run)
}

func (c *RunCoordinator) beginActiveLocked(personID string, run *activeRun) bool {
	if c.active == nil {
		c.active = make(map[string]map[*activeRun]struct{})
	}
	limit := c.activeLimit
	if limit < 1 {
		limit = 1
	}
	if len(c.active[personID]) >= limit {
		return false
	}
	// One CLI terminal has one foreground execution lane even when the person
	// owns several slots. A single IM chat deliberately has no such restriction:
	// its messages are routed by exact edge or Main coordination instead.
	if run != nil && run.Platform == "cli" && strings.TrimSpace(run.Channel) != "" {
		for existing := range c.active[personID] {
			if existing.Platform == "cli" && existing.Channel == run.Channel {
				return false
			}
		}
	}
	if c.active[personID] == nil {
		c.active[personID] = make(map[*activeRun]struct{})
	}
	c.active[personID][run] = struct{}{}
	return true
}

type activeRunContextKey struct{}

func withActiveRun(ctx context.Context, active *activeRun) context.Context {
	return context.WithValue(ctx, activeRunContextKey{}, active)
}

func (c *RunCoordinator) updateActive(ctx context.Context, task *control.Task, run *control.Run) {
	active, _ := ctx.Value(activeRunContextKey{}).(*activeRun)
	if active == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, registered := c.active[active.PersonID][active]; !registered {
		return
	}
	if task != nil {
		active.TaskID = task.ID
	}
	if run != nil {
		active.RunID = run.ID
		active.StartedAt = run.StartedAt
		active.ExecutionRoots = executionenv.CloneRootBindings(run.ExecutionRoots)
		// A continuation names the person's request, not its scheduling text.
		if summary := strings.TrimSpace(run.InputSummary); summary != "" {
			active.Summary = run.InputSummary
		}
	}
}

// activeForRun never falls back to another Run when an explicit target is
// missing or stale. This is the only safe lookup for cross-window controls.
func (c *RunCoordinator) activeForRun(personID, runID string) *activeRun {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for active := range c.active[personID] {
		if active.RunID == runID {
			copy := *active
			return &copy
		}
	}
	return nil
}

func (c *RunCoordinator) hasActiveTask(personID, taskID string) bool {
	if strings.TrimSpace(taskID) == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for active := range c.active[personID] {
		if active.TaskID == taskID {
			return true
		}
	}
	return false
}

// activeForIncoming applies only deterministic edges. CLI input belongs to its
// own foreground source; an exact reply belongs to its named Run. Ordinary
// input at any other endpoint never selects an execution parent by cardinality.
func (c *RunCoordinator) activeForIncoming(personID string, req api.MessageRequest) *activeRun {
	if runID := strings.TrimSpace(req.ReplyToRunID); runID != "" {
		return c.activeForRun(personID, runID)
	}
	if req.Platform != "cli" || strings.TrimSpace(req.Channel) == "" || strings.TrimSpace(req.PlatformUserID) == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for active := range c.active[personID] {
		if active.Platform == req.Platform && active.PlatformUserID == req.PlatformUserID && active.Channel == req.Channel {
			copy := *active
			return &copy
		}
	}
	return nil
}

func (c *RunCoordinator) activeCount(personID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active[personID])
}

func (c *RunCoordinator) hasCapacity(personID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := c.activeLimit
	if limit < 1 {
		limit = 1
	}
	return len(c.active[personID]) < limit
}

func (c *RunCoordinator) activeCapacity() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeLimit < 1 {
		return 1
	}
	return c.activeLimit
}

func (c *RunCoordinator) activeRunsForPerson(personID string) []*activeRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := make([]*activeRun, 0, len(c.active[personID]))
	for active := range c.active[personID] {
		copy := *active
		runs = append(runs, &copy)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.Before(runs[j].StartedAt)
	})
	return runs
}

func (c *RunCoordinator) stopActiveRun(personID, runID string) *activeRun {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for active := range c.active[personID] {
		if active.RunID != runID {
			continue
		}
		if active.Cancel != nil {
			active.Cancel()
		}
		copy := *active
		return &copy
	}
	return nil
}

func (c *RunCoordinator) endActiveRun(personID string, active *activeRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active[personID], active)
	if len(c.active[personID]) == 0 {
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
	statuses := make([]api.ActiveRunStatus, 0)
	for _, runs := range c.active {
		for active := range runs {
			status := formatActiveRunStatus(activeRunCopy(active))
			if status != nil {
				statuses = append(statuses, *status)
			}
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].PersonID != statuses[j].PersonID {
			return statuses[i].PersonID < statuses[j].PersonID
		}
		if statuses[i].StartedAt != statuses[j].StartedAt {
			return statuses[i].StartedAt < statuses[j].StartedAt
		}
		return statuses[i].RunID < statuses[j].RunID
	})
	return statuses
}

// stopAllActive interrupts every active run during gateway shutdown. This is
// infrastructure recovery, not a user cancellation: a bound queue row keeps
// its exact Run identity so boot recovery can observe effects before resuming.
func (c *RunCoordinator) stopAllActive(reason string) {
	c.mu.Lock()
	var runs []*activeRun
	for _, personRuns := range c.active {
		for active := range personRuns {
			copy := *active
			runs = append(runs, &copy)
		}
	}
	c.mu.Unlock()

	store := c.srv.Control
	for _, active := range runs {
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
	preparationStarted := time.Now()
	// Install the watchdog before installExecutionScope captures ctx for
	// approval and clarify callbacks. Previously the router created it later,
	// so a clarify wait paused a different context and the real watchdog killed
	// an otherwise healthy run after ten quiet minutes.
	ctx, stopWatchdog := router.PrepareRunWatchdog(ctx)
	defer stopWatchdog()
	d := c.srv
	// The async caller settles queue ownership. A created Run keeps its
	// durable finalizer; a failure before creation retains the unstarted input
	// instead of treating a worker's return as completed work.
	if _, err := c.prepareRequestWorkspace(ctx, identity, &req); err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: d.messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	task, attach, err := c.resolveTask(ctx, identity, req, intent)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.TrimSpace(req.ReplyToRunID) != "" || strings.TrimSpace(req.ApprovalID) != "" || strings.TrimSpace(req.ClarifyID) != "" {
			status = http.StatusConflict
		}
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: d.messageContextBudget(llmUsageZero())}, status
	}
	attach.effectKey = strings.TrimSpace(req.EffectKey)
	// Resolve the parent run READ-ONLY before anything is created: every full-context
	// channel (selector, resume block, loop checkpoint) receives this one
	// answer, and an ambiguous person-typed continuation stops here with a
	// deterministic candidate list instead of starting a model run under a
	// guessed parent.
	var parentRes resumeTargetResolution
	if task != nil && attach.resolvedPolicy().ContextMode == attachContextFull {
		if attach.resumesRunID != "" {
			// Structured evidence (reply/approval) named the exact run: resolve
			// only it — the task's other unresolved runs are irrelevant here.
			parentRes, err = c.resolveExplicitResumeTarget(ctx, identity, task, attach.resumesRunID)
		} else {
			parentRes, err = c.resolveResumeTarget(ctx, identity, task)
			if parentRes.ambiguous() && attach.selectsPriorRun() && isUserOriginTurn(ctx, req) {
				resp := c.resumeCandidatesResponse(ctx, identity, req, task, parentRes.candidates)
				if resp.Error != "" {
					return resp, http.StatusServiceUnavailable
				}
				return resp, http.StatusOK
			}
		}
		if err != nil {
			if req.QueueID != "" && req.QueueClaimToken != "" && req.ReplyToRunID != "" && isUserOriginTurn(ctx, req) {
				if released, releaseErr := c.returnUnboundQueueClaim(context.WithoutCancel(ctx), identity.TenantID, req.QueueID, req.QueueClaimToken); releaseErr == nil && released {
					return api.MessageResponse{Identity: identity, Turn: messageTurn("capacity_wait", "queued", "idle", task.ID, "", "exact continuation changed while queued")}, http.StatusOK
				}
			}
			return api.MessageResponse{Identity: identity, Task: task, Error: err.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", err.Error()), Context: d.messageContextBudget(llmUsageZero())}, http.StatusConflict
		}
	}
	workspace, workspaceErr := c.workspaceForTask(ctx, identity, task, req, attach)
	if workspaceErr != nil {
		return api.MessageResponse{Identity: identity, Task: task, Error: workspaceErr.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", workspaceErr.Error()), Context: d.messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	if rootsErr := c.prepareRequestExecutionRoots(ctx, workspace, &req); rootsErr != nil {
		return api.MessageResponse{Identity: identity, Task: task, Error: rootsErr.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", rootsErr.Error()), Context: d.messageContextBudget(llmUsageZero())}, http.StatusBadRequest
	}
	// Freeze the authority this Run executes under before it exists: the
	// snapshot a durable row restored, or a new admission built now. Every
	// later reader (scope, strategy, standing classes, outcome) takes it from
	// the request, and the Run row records it in its creating transaction.
	restoredAdmission := !req.Admission.IsZero()
	admission, admissionErr := c.requestAdmission(ctx, identity, req, intent)
	if admissionErr != nil {
		// Nothing was accepted: a claimed queue row goes back to the queue, and
		// the sender is told to try again. The mode is never guessed.
		if req.QueueID != "" && req.QueueClaimToken != "" {
			if released, releaseErr := c.returnUnboundQueueClaim(context.WithoutCancel(ctx), identity.TenantID, req.QueueID, req.QueueClaimToken); releaseErr == nil && released {
				if attach.created {
					_, _ = d.Control.DeleteEmptyTask(context.WithoutCancel(ctx), identity.TenantID, identity.PersonID, task.ID)
				}
				return api.MessageResponse{Identity: identity, Turn: messageTurn("capacity_wait", "queued", "idle", "", "", "the approval mode could not be read")}, http.StatusOK
			}
		}
		if attach.created {
			_, _ = d.Control.DeleteEmptyTask(context.WithoutCancel(ctx), identity.TenantID, identity.PersonID, task.ID)
		}
		resp, status, _ := modeRefusal(identity, admissionErr)
		return resp, status
	}
	req = underAdmission(req, admission)
	req.AdmissionRestored = restoredAdmission

	// A deliberate continuation claims its resolved parent in the SAME
	// transaction that creates the child run (P1: creation and ownership are
	// one atomic step; the unique parent index is the cross-process backstop).
	// Explicit task/resume attaches also claim an exact parent; ambiguity stops
	// before run creation.
	parent := parentRes.exact()
	claimParentID := ""
	if parent != nil && attach.claimsPriorRuns() && (isUserOriginTurn(ctx, req) || attach.reason == taskAttachApprovalResume || attach.reason == taskAttachClarifyResume || runOrigin(ctx, req) == runOriginRecovery || runOrigin(ctx, req) == runOriginWatch || runOrigin(ctx, req) == runOriginResource || runOrigin(ctx, req) == runOriginProviderWait) {
		claimParentID = parent.ID
	}
	exactContinuation := claimParentID != "" && exactContinuationEdge(ctx, req, attach)
	viewReady := c.maybeAssignIsolatedGitView(ctx, identity, task, &req, parent != nil)
	var run *control.Run
	if viewReady {
		c.setActiveExecutionRoots(ctx, req.ExecutionRoots)
		run, err = d.Control.StartRunWithOptions(ctx, task, req.Channel, runInputSummary(ctx, req), control.StartRunOptions{
			WorkKey:           attach.workKey,
			ExecutionRoots:    req.ExecutionRoots,
			ResumesRunID:      claimParentID,
			ExactContinuation: exactContinuation,
			QueueID:           req.QueueID,
			QueueClaimToken:   req.QueueClaimToken,
			MaxActiveRuns:     c.activeCapacity(),
			ExclusiveChannel:  req.Platform == "cli",
			Admission:         req.Admission,
		})
	} else {
		// Reuse the admission fallback below: a fresh input is durably
		// queued, and a drain releases its exact unbound claim.
		err = control.ErrRunCapacity
	}
	if errors.Is(err, control.ErrResumeTargetClaimed) || errors.Is(err, control.ErrResumeTargetNotResumable) {
		if c.handleObsoleteQueueClaim(context.WithoutCancel(ctx), identity, req) {
			return api.MessageResponse{Identity: identity, Task: task,
				Turn: messageTurn("stale_claim", "", "idle", task.ID, "", "The exact queued continuation did not start; its saved decision was retained.")}, http.StatusOK
		}
		if req.QueueID != "" && req.QueueClaimToken != "" && req.ReplyToRunID != "" && isUserOriginTurn(ctx, req) {
			released, releaseErr := c.returnUnboundQueueClaim(context.WithoutCancel(ctx), identity.TenantID, req.QueueID, req.QueueClaimToken)
			if releaseErr != nil {
				return api.MessageResponse{Identity: identity, Task: task, Error: releaseErr.Error(),
					Turn: messageTurn("failed", "", "idle", task.ID, "", releaseErr.Error())}, http.StatusServiceUnavailable
			}
			if released {
				return api.MessageResponse{Identity: identity, Turn: messageTurn("capacity_wait", "queued", "idle", task.ID, "", "exact continuation changed while queued")}, http.StatusOK
			}
			return api.MessageResponse{Identity: identity, Task: task,
				Turn: messageTurn("stale_claim", "", "idle", task.ID, "", "")}, http.StatusConflict
		}
		// A concurrent continuation claimed the parent first (or it stopped
		// being resumable). No fork: nothing was created; report the claimed
		// state deterministically instead of running under shared ownership.
		content := fmt.Sprintf("That work was just claimed by another continuation (%s). Use /status to see the active run, or /task %s runs to inspect the timeline.", shortRunID(claimParentID), shortTaskID(task.ID))
		return api.MessageResponse{
			Identity: identity, Task: task, Content: content,
			Turn: messageTurn("waiting_user", task.Status, "idle", task.ID, "", content),
		}, http.StatusOK
	}
	if err != nil {
		if attach.created {
			_, _ = d.Control.DeleteEmptyTask(context.WithoutCancel(ctx), identity.TenantID, identity.PersonID, task.ID)
		}
		if errors.Is(err, control.ErrRunCapacity) {
			if req.QueueID != "" {
				if _, releaseErr := c.returnUnboundQueueClaim(context.WithoutCancel(ctx), identity.TenantID, req.QueueID, req.QueueClaimToken); releaseErr != nil {
					return api.MessageResponse{Identity: identity, Error: releaseErr.Error(), Turn: messageTurn("failed", "", "idle", "", "", releaseErr.Error())}, http.StatusInternalServerError
				}
				return api.MessageResponse{Identity: identity, Turn: messageTurn("capacity_wait", "queued", "idle", "", "", "")}, http.StatusOK
			}
			queued, refusal := d.enqueueBehindActive(ctx, identity, req)
			if refusal != nil {
				// Never acknowledge input that nothing holds, and keep the
				// store's error out of what the sender sees.
				log.Warn("gateway: capacity fallback could not queue the input", "person", identity.PersonID, "error", refusal.cause)
				text := "This message could not be started because " + refusal.reason + ". Check /status before sending it again."
				return api.MessageResponse{Identity: identity, Error: text, Turn: messageTurn("failed", "", "idle", "", "", text)}, http.StatusServiceUnavailable
			}
			return queued, http.StatusOK
		}
		if errors.Is(err, control.ErrQueueClaimLost) {
			// The attempted Run was rolled back with the stale queue bind. The
			// newer owner will deliver its own result; this worker must neither
			// report a failure to the person nor settle the queue row.
			return api.MessageResponse{Identity: identity, Turn: messageTurn("stale_claim", "", "idle", "", "", "")}, http.StatusConflict
		}
		return api.MessageResponse{Identity: identity, Task: task, Error: err.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", err.Error()), Context: d.messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	// A refused insertion did not admit this input to a Run. Recording it on
	// every retry would duplicate the channel transcript and erase offer context.
	if runOrigin(ctx, req) != runOriginProviderWait {
		_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "user", req.Content)
	}
	stopHeartbeat := c.startRunHeartbeat(ctx, run, req.QueueID, req.QueueClaimToken)
	defer stopHeartbeat()
	c.updateActive(ctx, task, run)
	c.signalAsyncAdmission(ctx, identity, task, run)
	if ctx.Value(asyncAdmissionContextKey{}) != nil {
		stopProgressNotices := c.startAsyncProgressNotices(ctx, identity, req, run.ID)
		defer stopProgressNotices()
	}
	startedPayload := map[string]interface{}{
		"input":            truncate(req.Content, 500),
		"platform":         req.Platform,
		"platform_user_id": req.PlatformUserID,
		"presentation":     c.runPresentation(ctx, run, req),
		"resumes_run_id":   run.ResumesRunID,
		"approval_intent":  persistedApprovalIntent{Version: 3, Snapshot: c.intentSnapshotWithOffer(ctx, identity, task, run, workspace, req, req.Channel)},
	}
	if queueID := strings.TrimSpace(req.QueueID); queueID != "" {
		startedPayload["queue_id"] = queueID
	}
	if watchID := strings.TrimSpace(req.WatchID); watchID != "" {
		startedPayload["watch_id"] = watchID
		startedPayload["task_status"] = "running"
	}
	if origin := runOrigin(ctx, req); origin != "" {
		startedPayload["origin"] = origin
	}
	approvalID := strings.TrimSpace(req.ApprovalID)
	if approvalID != "" {
		startedPayload["source_approval_id"] = approvalID
	}
	if clarifyID := strings.TrimSpace(req.ClarifyID); clarifyID != "" {
		startedPayload["source_clarify_id"] = clarifyID
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.started",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(startedPayload),
	})
	if run.ResumesRunID != "" {
		_, _ = d.Control.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			RunID:      run.ID,
			Type:       "run.resumed",
			Visibility: "task",
			Channel:    req.Channel,
			Payload: mustJSON(map[string]interface{}{
				"resumes_run_id": run.ResumesRunID,
				"claimed":        true,
				"reason":         intent.Reason,
				"confidence":     intent.Confidence,
				"work_key":       strings.ToUpper(strings.TrimSpace(attach.workKey)),
			}),
		})
		// Restore the parent's plan before Main starts. This is durable child
		// state, not a display-only echo: completion checks and later resume paths
		// must see the same unresolved steps as the TUI checklist.
		if inherited, inheritErr := c.inheritResumeTargetPlan(ctx, identity, task, run); inheritErr != nil {
			log.Warn("resumed run plan inheritance failed", "resumes_run_id", run.ResumesRunID, "run_id", run.ID, "error", inheritErr)
		} else if inherited != nil {
			_, _ = d.Control.AppendEvent(ctx, control.Event{
				TaskID:     task.ID,
				RunID:      run.ID,
				Type:       "plan.updated",
				Visibility: "task",
				Channel:    req.Channel,
				Payload: mustJSON(map[string]interface{}{
					"plan":           tools.DisplayedPlanSteps(ctx, d.Control, identity.TenantID, run.ID, inherited.Plan.Steps),
					"explanation":    "Plan inherited from the continued run",
					"plan_version":   inherited.Plan.Version,
					"source":         "resumed_run",
					"resumes_run_id": run.ResumesRunID,
				}),
			})
		}
	}
	if attach.workKey != "" {
		d.appendLabelAssignedEvent(ctx, task.ID, run.ID, map[string]interface{}{
			"decision": "ingress_work_key",
			"work_key": attach.workKey,
			"task_id":  task.ID,
			"run_id":   run.ID,
		})
	}

	analysisWorkspaceID := run.WorkspaceID
	if workspace != nil && workspace.ID != "" {
		analysisWorkspaceID = workspace.ID
	}
	replay := runMaintenanceReplay{WorkspaceID: analysisWorkspaceID, UserInput: req.Content, Attach: attach}
	if runOrigin(ctx, req) == runOriginProviderWait {
		replay.UserInput = ""
	}
	if unavailableRoot, statErr := executionRootUnavailable(ctx, req.ExecutionRoots); statErr != nil {
		summary := fmt.Sprintf("The workspace environment is unavailable: %s", unavailableRoot)
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
			Context: d.messageContextBudget(llmUsageZero()),
		}, http.StatusOK
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
				Context: d.messageContextBudget(llmUsageZero()),
			}, http.StatusOK
		}
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, fmt.Errorf("materialize execution lease: %w", leaseErr), replay)
		content, errorText := interruptedRunResponse(task.Title, outcome)
		return api.MessageResponse{
			Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Error: errorText,
			Turn:    messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary),
			Context: d.messageContextBudget(llmUsageZero()),
		}, http.StatusOK
	}
	// Import attachment files into the daemon-managed person partition BEFORE
	// scope install and context assembly: the rendered attachment paths and
	// the scope's allowed roots must both point at the managed copies.
	req.Attachments = c.importAttachments(ctx, identity, run, req.Attachments)
	cleanupScope := c.installExecutionScope(ctx, identity, task, run, workspace, req, lease)
	defer cleanupScope()
	// Tag the turn with its run-scoped key so tool calls resolve exactly this
	// run's scope instead of the person's most recent one.
	if run != nil {
		ctx = tools.WithExecutionScopeKey(ctx, tools.ExecutionScopeKeyForRun(run.ID))
	}
	runScopeKey := ""
	if run != nil {
		runScopeKey = tools.ExecutionScopeKeyForRun(run.ID)
	}
	invocationScope := kernel.ToolInvocationScope{
		ControlTenantID:   identity.TenantID,
		PersonID:          identity.PersonID,
		AccountID:         identity.AccountID,
		TaskID:            task.ID,
		WorkspaceID:       req.WorkspaceID,
		ExecutionScopeKey: runScopeKey,
		ExecutionLane:     "main",
		AttachmentMode:    string(attach.reason),
		RecoveryMode:      strings.TrimSpace(req.RecoveryMode),
		SkillMutationMode: kernel.SkillMutationCandidateOnly,
	}
	if run != nil {
		invocationScope.RunID = run.ID
		invocationScope.WorkUnitID = run.WorkUnitID
	}
	if workspace != nil {
		invocationScope.WorkspaceID = workspace.ID
	}
	if lease != nil {
		invocationScope.LeaseID = lease.ID
		invocationScope.EnvironmentGeneration = lease.EnvironmentGeneration
	}
	ctx = kernel.WithToolInvocationScope(ctx, invocationScope)
	ctx = kernel.WithRunExecutionState(ctx, kernel.NewRunExecutionState())
	if run != nil {
		ctx = tools.WithRunPlanProjection(ctx, c.newRunPlanProjection(identity, run, invocationScope))
	}
	if workspace != nil && workspace.LocalPath != "" {
		contextRoots := executionenv.ContextRootPaths(req.ExecutionRoots)
		primaryRoot := workspace.LocalPath
		for _, binding := range req.ExecutionRoots {
			if binding.Role == executionenv.RootRolePrimary {
				primaryRoot = binding.Path
				break
			}
		}
		ctx = kernel.WithWorkspaceContext(ctx, kernel.WorkspaceContext{
			ID:    workspace.ID,
			Root:  primaryRoot,
			Roots: contextRoots,
		})
	}
	if selected := c.selectSkillRuntimeContext(ctx, identity, task, run, attach, req.Content); selected.Active != nil || len(selected.Candidates) > 0 {
		ctx = kernel.WithSkillRuntimeContext(ctx, selected)
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
	ctx = c.withLoopCheckpointResume(ctx, identity, task, parent, intent)
	if strings.HasPrefix(req.EffectKey, "provider-wait:") {
		if parent == nil || req.EffectKey != "provider-wait:"+parent.ID || !kernel.HasLoopResumeMessages(ctx) {
			err := fmt.Errorf("%w for its exact parent; work was not replayed", errProviderWaitCheckpoint)
			outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, err, replay)
			content, errorText := interruptedRunResponse(task.Title, outcome)
			return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content,
				Error: errorText, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary),
				Context: d.messageContextBudget(llmUsageZero())}, http.StatusOK
		}
		ctx = kernel.WithExactLoopReplay(ctx)
	}
	if strings.HasPrefix(req.EffectKey, "execution-lane:") {
		var resumeErr error
		ctx, resumeErr = c.withExecutionWaitResume(ctx, identity, parent, req.EffectKey)
		if resumeErr != nil {
			outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, resumeErr, replay)
			content, errorText := interruptedRunResponse(task.Title, outcome)
			return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Error: errorText, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary)}, http.StatusOK
		}
	}
	// Restore and validate the exact checkpoint before recall. Human/provider
	// recovery then skips only semantic expansion, retaining current scope,
	// lexical references and the normal foreground context for ordinary input.
	runtimeContext := c.selectedTaskRuntimeContextWithMode(ctx, task, run, workspace, req.Platform, req.Channel, req.Content, attach.resolvedPolicy().ContextMode, parent)
	if isUserOriginTurn(ctx, req) && !req.ForceNew && attach.reason == taskAttachNewLabel &&
		strings.TrimSpace(req.ReplyToRunID) == "" && strings.TrimSpace(req.ApprovalID) == "" && strings.TrimSpace(req.ClarifyID) == "" {
		runtimeContext.WorkContinuityHints = c.workContinuityHints(ctx, identity, run, req.Channel, 3)
	}
	ctx = kernel.WithTaskRuntimeContext(ctx, runtimeContext)
	agentInput := c.withGatewayContext(req.Content, identity, task, workspace, req.ExecutionRoots, req.Attachments)
	agentInput = c.withResumeContext(ctx, identity, task, parent, intent, attach.claimsPriorRuns(), agentInput)
	// Independent of continuation intent: any run on a task with uncertain
	// (crash-orphaned) side-effect tool calls must verify before repeating
	// (P0-B closure — a boot-requeued run re-drains as a "new" message).
	agentInput = c.withUncertainToolWarning(ctx, identity, task, agentInput)
	// The tool surface is the admitted one: a continuation's scheduling text
	// is never re-classified, so it can neither earn nor lose web access or a
	// verification-only surface the work was admitted with.
	strategy := admittedStrategy(req.Admission)
	if !restoredAdmission {
		strategy.Reason = taskStrategyForRequest(req, intent).Reason
	}
	strategy.ExecSandboxNote = tools.ExecSandboxPromptNoteForContext(ctx)
	ctx = kernel.WithTaskStrategy(ctx, strategy)
	completion := &runCompletionSession{coordinator: c, identity: identity, task: task, run: run, req: req, replay: replay, waiters: make(map[string]chan error)}
	ctx = context.WithValue(ctx, runCompletionSessionKey{}, completion)
	ctx = tools.WithRunCompletionSink(ctx, completion)
	ctx = kernel.WithCommittedTurnReader(ctx, completion)
	// The work spine records what the person asked for. A daemon continuation
	// runs on scheduling text, so it carries the admitted request instead, and
	// scheduled work keeps its source tag however it reached a worker.
	if continuesParkedWork(ctx, req) && strings.TrimSpace(req.Admission.Input.Summary) != "" {
		ctx = kernel.WithSpineRequest(ctx, req.Admission.Input.Summary)
	}
	// The tag follows the authority the work was admitted with, not the hop
	// that runs it: a provider wait or an answered approval of a scheduled
	// fire is still the schedule's work.
	if (runOrigin(ctx, req) == runOriginCron || req.Admission.Origin.Kind == runOriginCron) && kernel.TurnSourceFromContext(ctx) == "" {
		ctx = kernel.WithTurnSource(ctx, runOriginCron)
	}

	if d.Gateway == nil {
		err := fmt.Errorf("gateway is not configured")
		outcome := api.RunOutcome{Status: "failed", CompletionReason: "gateway_unavailable", Summary: err.Error(), Risks: []string{err.Error()}}
		_ = c.materializeRunFinalization(context.Background(), identity, task, run, "failed", replay.WorkspaceID, replay.UserInput, req.Channel, "", outcome, replay.Attach,
			control.Handoff{TaskID: task.ID, Summary: outcome.Summary, Risks: outcome.Risks},
			control.Event{Type: "run.failed", Visibility: "task", Channel: req.Channel, Payload: mustJSON(map[string]interface{}{"outcome": outcome, "error": err.Error()})})
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn("failed", "failed", "idle", task.ID, run.ID, err.Error()), Context: d.messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}

	_, _ = d.Control.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "run.prepared", Visibility: "task", Channel: req.Channel,
		Payload: mustJSON(map[string]interface{}{"duration_ms": time.Since(preparationStarted).Milliseconds(), "exact_checkpoint_replay": kernel.HasExactLoopReplay(ctx)})})
	resp, err := d.Gateway.RunAgentWithEvents(ctx, identity.PersonID, req.Channel, agentInput)
	if err != nil {
		var providerWait *llm.ProviderWait
		if errors.As(err, &providerWait) && ctx.Err() == nil {
			if outcome, parkErr := c.parkProviderWait(ctx, identity, task, run, req, providerWait); parkErr == nil {
				content := outcome.Summary + " SelfMind will continue this exact work automatically."
				return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content,
					Turn:    messageTurn("waiting_external", "in_progress", "idle", task.ID, run.ID, outcome.Summary),
					Context: d.messageContextBudget(llmUsageZero())}, http.StatusOK
			} else {
				err = fmt.Errorf("provider wait could not be parked safely: %w", parkErr)
			}
		}
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, err, replay)
		content, errorText := interruptedRunResponse(task.Title, outcome)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Error: errorText, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: d.messageContextBudget(llmUsageZero())}, http.StatusOK
	}

	content, usage, eventSummary, hasFinalContent, err := c.aggregateGatewayResponse(ctx, req.Channel, task, run, resp)
	if committed := completion.response(usage); committed != nil {
		return *committed, http.StatusOK
	}
	if err != nil {
		var providerWait *llm.ProviderWait
		if errors.As(err, &providerWait) && ctx.Err() == nil {
			if outcome, parkErr := c.parkProviderWait(ctx, identity, task, run, req, providerWait); parkErr == nil {
				content := outcome.Summary + " SelfMind will continue this exact work automatically."
				return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage,
					Turn:    messageTurn("waiting_external", "in_progress", "idle", task.ID, run.ID, outcome.Summary),
					Context: d.messageContextBudget(usage)}, http.StatusOK
			} else {
				err = fmt.Errorf("provider wait could not be parked safely: %w", parkErr)
			}
		}
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, err, replay)
		content, errorText := interruptedRunResponse(task.Title, outcome)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage, Error: errorText, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: d.messageContextBudget(usage)}, http.StatusOK
	}

	// Finalization must survive turn cancellation. The turn ctx can be
	// cancelled or deadline-expired in the window between the agent stream
	// completing and these writes (observed with eval turn budgets); using the
	// live ctx here made FinishRun/UpdateTaskStatus fail silently and left runs
	// stuck in `running` forever. WithoutCancel keeps ctx values (observers,
	// scopes) while guaranteeing the terminal state lands in control.db.
	if ctx.Err() != nil {
		outcome := c.finalizeErroredRun(ctx, identity, task, run, req.Channel, ctx.Err(), replay)
		content, errorText := interruptedRunResponse(task.Title, outcome)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage, Error: errorText, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: d.messageContextBudget(usage)}, http.StatusOK
	}
	finCtx := context.WithoutCancel(ctx)
	// A non-streaming provider never passes through the per-event refresh, so
	// re-read the run once more: a same-domain direct claim made by work_select
	// moved it onto the parent's thread and finalization must follow.
	c.refreshDirectContinuation(finCtx, task, run)
	selection, selectionErr := c.commitWorkSelection(finCtx, identity, req, task, run)
	if selectionErr != nil {
		selection = &workSelectionCommit{
			Rejected: true,
			Notice:   "I found related historical work, but the gateway could not validate a safe continuation. Nothing was attached or started: " + tools.RedactSensitive(selectionErr.Error()),
		}
	}
	if selection != nil {
		if selection.Rejected {
			content = selection.Notice
		} else if selection.Action == "resume" && strings.TrimSpace(selection.Notice) != "" && !strings.Contains(content, selection.Notice) {
			content = strings.TrimSpace(content)
			if content != "" {
				content += "\n\n"
			}
			content += selection.Notice
		}
	}
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
	outcome = reconcileMissingFinalResponse(outcome, structuredOutcome, hasFinalContent)
	if selection != nil && selection.Rejected {
		outcome.Status = "waiting_user"
		outcome.CompletionReason = "work_selection_rejected"
		outcome.Resumable = true
		outcome.Summary = selection.Notice
		outcome.NextSteps = []string{"Confirm whether to continue the historical work separately. No continuation was queued."}
	}
	verification, evidenceFiles := c.evidenceOutcome(finCtx, task.TenantID, run.ID)
	outcome.Verification, outcome.Files = verification, evidenceFiles
	outcome.ClaimMismatches = verificationClaimMismatches(outcome)
	outcome = applyVerificationOutcome(outcome)
	if !hasFinalContent && !structuredOutcome && (selection == nil || !selection.Rejected) {
		content, outcome.Summary = missingFinalEvidenceSummary(run.ID, outcome, c.acceptedPlanSteps(finCtx, identity.TenantID, run.ID))
	}
	if watchID := strings.TrimSpace(req.WatchID); watchID != "" {
		if watch, watchErr := d.Control.GetExternalWatch(finCtx, identity.TenantID, watchID); watchErr == nil {
			outcome = reconcileExternalWatchOutcome(outcome, watch)
		} else {
			log.Warn("external watch outcome lookup failed", "watch_id", watchID, "run_id", run.ID, "error", watchErr)
		}
	}
	for _, mismatch := range outcome.ClaimMismatches {
		outcome.Risks = appendUnique(outcome.Risks, mismatch, 8)
	}
	if structuredOutcome && !hasFinalContent {
		content = structuredResultFallback(outcome)
	}
	if !structuredOutcome && eventSummary.Completion().CompletionReason == "plan_unresolved" {
		content, outcome.Summary = unresolvedPlanFallback(outcome, c.acceptedPlanSteps(finCtx, identity.TenantID, run.ID))
	}
	content = withVerificationNotice(content, outcome.Verification, outcome.ClaimMismatches)
	content = withCompletionNotice(content, outcome)
	content = c.withExternalEffectFacts(finCtx, identity.TenantID, identity.PersonID, run.ID, content, &outcome)
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
	// non-terminal (the continuation ladder keeps offering its runs for `继续`/`/resume`)
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
	finalizeErr := c.materializeRunFinalization(finCtx, identity, task, run, taskStatus,
		replay.WorkspaceID, replay.UserInput, req.Channel, content, outcome, replay.Attach, handoff, terminalEvent)
	if errors.Is(finalizeErr, control.ErrRunCancellationRequested) {
		outcome := c.finalizeErroredRun(finCtx, identity, task, run, req.Channel, context.Canceled, replay)
		content, errorText := interruptedRunResponse(task.Title, outcome)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage, Error: errorText, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: d.messageContextBudget(usage)}, http.StatusOK
	}
	recordFinalizeErr("materialize run finalization", finalizeErr)
	if finalizeErr != nil {
		outcome := c.finalizeErroredRun(finCtx, identity, task, run, req.Channel, fmt.Errorf("%w: %v", errRunResultCommit, finalizeErr), replay)
		// This request already executed. Return its structured unresolved
		// outcome, as for other post-start failures, rather than invite an HTTP
		// retry that could repeat effects.
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: outcome.Summary + " " + strings.Join(outcome.NextSteps, " "), Usage: usage,
			Error: outcome.Summary, Turn: messageTurn(outcome.Status, outcome.Status, "idle", task.ID, run.ID, outcome.Summary), Context: d.messageContextBudget(usage)}, http.StatusOK
	}
	if selection != nil && !selection.Rejected && !selection.Direct {
		recordFinalizeErr("project work interaction", d.Control.ProjectInteractionTask(finCtx, identity.TenantID, identity.PersonID, task.ID, run.ID))
	}
	c.recordRecallOutputOverlap(finCtx, task, run, req.Channel, content)
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
	out := api.MessageResponse{Identity: identity, Task: refreshed, Run: run, Outcome: &outcome, Content: content, Usage: usage, Turn: messageTurn(turnStatus, taskStatus, "idle", taskID, run.ID, outcome.Summary), Context: d.messageContextBudget(usage)}
	if selection != nil && !selection.Rejected {
		out.Turn.QueueID = strings.TrimSpace(selection.QueueID)
	}
	return out, http.StatusOK
}

func (c *RunCoordinator) inheritResumeTargetPlan(ctx context.Context, identity *control.IdentityContext, task *control.Task, child *control.Run) (*control.RunPlanProjection, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || child == nil || strings.TrimSpace(child.ResumesRunID) == "" {
		return nil, nil
	}
	projection, err := c.srv.Control.EnsureInheritedRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil {
		return nil, err
	}
	if projection != nil {
		return projection, nil
	}
	steps := make([]control.RunPlanStepInput, 0)
	// Legacy Runs may only have a plan.updated event. This path cannot assert
	// source step identity or carry verification evidence.
	for _, step := range c.srv.latestPlanForRun(ctx, identity.TenantID, identity.PersonID, task.ID, child.ResumesRunID) {
		steps = append(steps, control.RunPlanStepInput{Step: step.Step, Status: step.Status})
	}
	if len(steps) == 0 {
		return nil, nil
	}
	legacy, err := c.srv.Control.SyncRunPlan(ctx, identity.TenantID, child.ID, "Plan inherited from the continued run", steps)
	if err != nil {
		return nil, err
	}
	return &legacy, nil
}

// Origins of a run the daemon started on the person's behalf. A turn the
// person typed at any endpoint has no origin: it is their own foreground work,
// wherever they typed it.
const (
	runOriginWatch        = "watch"
	runOriginCron         = "cron"
	runOriginApproval     = "approval"
	runOriginRecovery     = "recovery"
	runOriginResource     = "resource"
	runOriginProviderWait = "provider_wait"
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

// isUserOriginTurn reports whether the person themself typed this turn.
// Daemon-originated turns (cron, watch finalization, approval resume) must
// never steer the person's active run or trigger ambiguous-continuation
// prompts just because their generated text happens to contain a cue word —
// they queue durably like any other system work.
func isUserOriginTurn(ctx context.Context, req api.MessageRequest) bool {
	return runOrigin(ctx, req) == ""
}

// startAsyncRun accepts a turn immediately and runs it in the background,
// delivering the result back to the source channel when it finishes. When the
// slot was taken meanwhile the turn is queued instead, and a failed queue
// write is returned as a refusal.
func (c *RunCoordinator) startAsyncRun(identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (api.MessageResponse, *notDispatched) {
	baseCtx := c.srv.BackgroundRunContext
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	runCtx, cancelCause := context.WithCancelCause(baseCtx)
	runCancel := func() { cancelCause(context.Canceled) }
	active := &activeRun{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Channel:        req.Channel,
		Platform:       req.Platform,
		PlatformUserID: req.PlatformUserID,
		WorkspaceID:    req.WorkspaceID,
		ApprovalMode:   req.ApprovalMode,
		Origin:         runOrigin(context.Background(), req),
		ExecutionRoots: executionenv.CloneRootBindings(req.ExecutionRoots),
		QueueID:        req.QueueID,
		Summary:        truncate(req.Content, 240),
		StartedAt:      time.Now(),
		Cancel:         runCancel,
		Interrupt:      cancelCause,
		// Registered here so /v1/runs/steer can inject guidance; wired into the
		// run ctx below so the agent loop drains it at iteration boundaries.
		Steer: make(chan kernel.SteeringInput, steerBufferSize),
	}
	if ok, response, refusal := c.admitActive(runCtx, identity, req, active); !ok {
		runCancel()
		return response, refusal
	}
	runCtx = kernel.WithSteeringInputs(runCtx, active.Steer)
	admitted := make(chan api.MessageResponse, 1)
	runCtx = context.WithValue(runCtx, asyncAdmissionContextKey{}, admitted)
	go func() {
		response := api.MessageResponse{Identity: identity, Error: "The run could not be started.",
			Turn: messageTurn("failed", "", "idle", "", "", "The run could not be started.")}
		// A pre-Run refusal reaches the caller only after cleanup released its
		// slot. Successful Run insertion still signals admission immediately.
		defer func() {
			select {
			case admitted <- response:
			default:
			}
		}()
		// Defers run LIFO: drainQueue is registered first so it runs LAST —
		// after endActive frees the per-person slot — chaining the next queued
		// item into its own async run once this one is truly done.
		defer func() {
			// A pre-Run refusal is retried by the periodic scheduler, not this
			// finalizer chain. Otherwise a persistent startup fault can spin.
			if active.RunID != "" {
				c.drainQueue(identity)
			}
		}()
		defer c.endActiveRun(identity.PersonID, active)
		// Register after endActive so LIFO executes this first: acknowledged
		// input is durable in the next-turn queue before the active slot becomes
		// visible as idle to another endpoint.
		defer c.deferUnconsumedSteering(identity, active)
		defer runCancel()
		// Panic firewall: async runs (IM, queue drain, cron, detached CLI) have
		// no net/http per-request recover shielding them, so an unrecovered panic
		// in runMessage would crash the ENTIRE gateway daemon. Registered LAST so
		// it unwinds FIRST — before endActive/drainQueue — letting it read the
		// active-run registry to finalize the run/task while the slot still
		// exists. endActive + drainQueue then still run, so the person is never
		// left wedged behind a dead run.
		defer func() {
			if r := recover(); r != nil {
				if active.RunID == "" && req.QueueID != "" {
					log.Error("queued startup panicked", "queue_id", req.QueueID, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					c.retainQueuedStartupFailure(identity, req, "An internal error interrupted startup.")
					return
				}
				accepted := c.recoverAsyncRun(identity, req, active, r)
				c.settleAsyncQueue(identity, req, accepted)
			}
		}()
		resp, _ := c.runMessage(withActiveRun(runCtx, active), identity, req, intent)
		response = resp
		if req.QueueID != "" && active.RunID == "" && resp.Error != "" {
			c.retainQueuedStartupFailure(identity, req, resp.Error)
			return
		}
		if resp.Turn != nil && (resp.Turn.Status == "stale_claim" || resp.Turn.Status == "capacity_wait") {
			return
		}
		// Before a fresh Run exists this is the admission response the caller
		// is still awaiting. A second async push would repeat the same refusal
		// or queue acknowledgement. Already-accepted queue rows still need
		// their terminal result delivered even when preparation failed.
		if req.QueueID == "" && active.RunID == "" {
			return
		}
		if resp.Choice != nil && active.RunID == "" {
			// The choice, its notification intent and the exact queue handoff
			// committed together. No second settlement or async push is needed.
			return
		}
		accepted := c.deliverAsyncResult(context.Background(), identity, req, resp)
		c.settleAsyncQueue(identity, req, accepted)
	}()
	response := <-admitted
	if !response.Accepted || response.Turn == nil || response.Turn.Status != "accepted" {
		return response, nil
	}

	notice := router.WorkingNotice(req.Channel)
	if notice == "" {
		notice = "Started in the background. Use /status to check progress or /stop to cancel."
	}
	response.Content = notice
	response.Turn = messageTurn("accepted", "running", "running", response.Turn.TaskID, response.Turn.RunID, notice)
	return response, nil
}

// recoverAsyncRun contains a panicked async run so the daemon survives. It logs
// the panic with its stack, then reuses the ordinary failure-finalize contract:
// the run is marked failed and the task interrupted (non-terminal/resumable), so
// status, the active-run registry, and the queue stay consistent — the caller's
// deferred endActive + drainQueue still run afterward, freeing the person's slot
// so they are not wedged. It never re-panics. Run/task ids come from the active
// registry snapshot (still present because this defer unwinds before endActive).
func (c *RunCoordinator) recoverAsyncRun(identity *control.IdentityContext, req api.MessageRequest, active *activeRun, r interface{}) bool {
	log.Error("async run panicked; recovered to keep the gateway alive",
		"person", identity.PersonID, "channel", req.Channel,
		"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
	if c.srv == nil || c.srv.Control == nil {
		return false
	}
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
	if req.QueueClaimToken != "" {
		if _, err := c.srv.Control.FinishQueuedClaim(context.Background(), identity.TenantID, req.QueueID, req.QueueClaimToken, control.QueueStatusDone); err != nil {
			log.Warn("gateway: settle claimed queue row failed", "queue_id", req.QueueID, "error", err)
		}
	} else {
		_, _ = c.srv.Control.MarkQueuedIfStatus(context.Background(), identity.TenantID, req.QueueID, control.QueueStatusStarted, control.QueueStatusDone)
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
	// A safe process drain leaves queued rows durable for the next daemon. A
	// run finalizer must not race the restart by launching the next item under
	// the old model/runtime after shutdown has already begun.
	if c.srv.IsDraining() || !c.srv.modelReadyForWork() {
		return
	}
	personID := identity.PersonID
	c.mu.Lock()
	limit := c.activeLimit
	if limit < 1 {
		limit = 1
	}
	if len(c.active[personID]) >= limit { // a run raced in; its own finalize will drain
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
	startedRun := false
	defer func() {
		c.mu.Lock()
		delete(c.draining, personID)
		c.mu.Unlock()
		// A newly launched Run may leave another slot free. Only a successful
		// launch schedules a further drain, so an unavailable model or poisoned
		// queue row cannot create a spin loop.
		if startedRun && c.hasCapacity(personID) {
			go c.drainQueue(identity)
		}
	}()

	ctx := context.Background()
	// The process registry is a cancel/steer handle, not the admission source.
	// An older daemon may have left a running Run whose effects are uncertain;
	// it must be recovered before its slot can be reused.
	hostWaits, err := c.srv.refreshExecutionWaits(ctx, identity.TenantID, personID)
	if err != nil {
		log.Warn("gateway: cannot inspect managed execution waits", "person_id", personID, "error", err)
		return
	}
	reservations, err := c.srv.Control.ListRunRootReservations(ctx, identity.TenantID, personID)
	workRuns := len(reservations)
	if err != nil || workRuns >= limit {
		if err != nil {
			log.Warn("gateway: cannot inspect durable run capacity before queue drain", "person_id", personID, "error", err)
		}
		return
	}
	var next *control.QueuedTask
	var claimToken string
	// Rows this drain found it may not start because the authority they would
	// run under cannot be read. They hold their source like an unreadable row.
	held := map[string]bool{}
	heldSources := map[string]bool{}
	for {
		// Resolve exact reply lineage before opening NextQueuedWhere's cursor:
		// control.db uses one connection and the predicate cannot query it.
		// A waiting reply blocks its source, not other independent sources.
		rows, err := c.srv.Control.ListDueQueuedContinuations(ctx, identity.TenantID, personID)
		if err != nil {
			return
		}
		blockedContinuation := map[string]bool{}
		for _, row := range rows {
			_, waiting, resolveErr := c.srv.Control.ResolveQueuedContinuation(ctx, row)
			var obsolete *control.QueuedContinuationObsolete
			if errors.As(resolveErr, &obsolete) {
				if !c.retireObsoleteContinuation(ctx, identity, row, obsolete) {
					held[row.ID] = true
				}
				continue
			}
			if waiting || resolveErr != nil {
				blockedContinuation[row.ID] = true
				if resolveErr != nil {
					log.Warn("gateway: queued continuation cannot resolve exact lineage", "queue_id", row.ID, "error", resolveErr)
				}
			}
		}
		active := c.activeRunsForPerson(personID)
		blockedChannels := map[string]bool{}
		blockedParents := map[string]map[string]bool{}
		var unreadable []control.QueuedTask
		next, err = c.srv.Control.NextQueuedWhere(ctx, identity.TenantID, personID, func(q control.QueuedTask) bool {
			source := queuedSourceKey(q)
			if heldSources[source] {
				return false
			}
			if q.AdmissionError != nil || held[q.ID] {
				// The store never launches this row and holds its source; the
				// person is told why once the cursor is closed.
				if q.AdmissionError != nil {
					unreadable = append(unreadable, q)
				}
				blockedChannels[source] = true
				return false
			}
			// A user reply to a parked Run waits for its exact system child.
			// Let that finalization pass the reply in the same source, or strict
			// source FIFO would deadlock the prerequisite behind its dependent.
			prerequisite := q.Class == control.QueueClassFinalization && q.IdempotencyKey != "" && blockedParents[source][q.ReplyToRunID]
			if blockedChannels[source] && !prerequisite {
				return false
			}
			if blockedContinuation[q.ID] || !c.queuedExecutionLaneReady(q, hostWaits) || !queuedResourcesReady(q, active, reservations) {
				blockedChannels[source] = true
				if blockedContinuation[q.ID] && q.ReplyToRunID != "" {
					if blockedParents[source] == nil {
						blockedParents[source] = map[string]bool{}
					}
					blockedParents[source][q.ReplyToRunID] = true
				}
				return false
			}
			return true
		})
		for _, row := range unreadable {
			c.noticeUnreadableAdmission(ctx, identity, row, row.AdmissionError)
		}
		if err != nil || next == nil {
			return
		}
		// A daemon continuation recorded without a snapshot continues its
		// exact parent's authority when the parent has one. A parent whose
		// snapshot cannot be read stops the row rather than letting it run
		// under a re-derived, possibly wider, authority.
		if next.Admission.IsZero() && daemonContinuationKey(next.IdempotencyKey) {
			admission, admissionErr := c.srv.continuationAdmission(ctx, *next)
			if errors.Is(admissionErr, control.ErrAdmissionSnapshotUnreadable) {
				held[next.ID] = true
				c.noticeUnreadableAdmission(ctx, identity, *next, admissionErr)
				continue
			}
			if admissionErr != nil {
				log.Warn("gateway: queued continuation cannot read its parent's admission", "queue_id", next.ID, "error", admissionErr)
				return
			}
			next.Admission = admission
		}
		// A row with no snapshot (accepted before schema v26) and no mode of its
		// own takes the person's mode when it starts, and that mode is then frozen
		// for its lineage. When it cannot be read the row is not claimed: it stays
		// queued, holding its source for this drain, and the next drain tries
		// again. It never starts under a guess, and nothing spins.
		if next.Admission.IsZero() && strings.TrimSpace(next.ApprovalMode) == "" {
			if _, modeErr := c.resolveApprovalMode(ctx, identity, ""); modeErr != nil {
				held[next.ID] = true
				continue
			}
		}
		// A scheduled fire is authorized by the schedule the person created, and
		// the queue can hold it until that schedule is deleted or switched off.
		// Ask before claiming: a gone schedule's fire is dropped with a notice, a
		// lookup that fails leaves the row queued and unclaimed (this drain
		// skips it, the next tick asks again), and nothing spins.
		if next.Admission.Origin.Kind == runOriginCron {
			verdict, why, lookupErr := c.srv.queuedCronFire(ctx, *next)
			switch verdict {
			case cronFireDropped:
				if !c.dropOrphanedCronFire(ctx, identity, *next, why) {
					held[next.ID] = true
				}
				continue
			case cronFireUnknown:
				if _, logged := c.cronLookupReported.LoadOrStore(next.ID, true); !logged {
					log.Warn("gateway: could not check the schedule of a queued scheduled fire; it stays queued until the lookup works",
						"queue_id", next.ID, "job_id", next.Admission.Origin.ID, "error", lookupErr)
				}
				held[next.ID] = true
				continue
			}
		}
		// The operational recovery rollback applies at both creation and claim
		// time. A row scheduled before the daemon restarted with the switch off
		// must not slip through merely because it is already durable.
		if c.srv.DisableAutomaticRunRecovery && next.Class == control.QueueClassRecovery {
			if c.srv.cancelDisabledRecoveryQueue(ctx, identity, next) {
				continue
			}
			return
		}
		var claimed bool
		claimToken, claimed, err = c.srv.Control.ClaimQueued(ctx, identity.TenantID, next.ID, 0)
		if err != nil || !claimed {
			return
		}
		if next.ReplyToRunID != "" {
			resolved, wait, resolveErr := c.srv.Control.ResolveQueuedContinuation(ctx, *next)
			var obsolete *control.QueuedContinuationObsolete
			if errors.As(resolveErr, &obsolete) {
				next.ClaimToken = claimToken
				if !c.retireObsoleteContinuation(ctx, identity, *next, obsolete) {
					held[next.ID] = true
					if _, releaseErr := c.returnUnboundQueueClaim(ctx, next.TenantID, next.ID, claimToken); releaseErr != nil {
						log.Warn("gateway: release obsolete continuation claim failed", "queue_id", next.ID, "error", releaseErr)
					}
				}
				continue
			}
			if resolveErr != nil || wait {
				if _, releaseErr := c.returnUnboundQueueClaim(ctx, next.TenantID, next.ID, claimToken); releaseErr != nil {
					log.Warn("gateway: release waiting continuation claim failed", "queue_id", next.ID, "error", releaseErr)
				}
				if resolveErr != nil {
					log.Warn("gateway: queued continuation cannot resolve exact lineage", "queue_id", next.ID, "error", resolveErr)
				}
				return
			}
			next = &resolved
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

		req := api.MessageRequest{
			TenantID:       next.TenantID,
			Platform:       next.Platform,
			PlatformUserID: next.PlatformUserID,
			Channel:        next.Channel,
			Content:        next.Content,
			ApprovalMode:   next.ApprovalMode,
			WorkspaceID:    next.WorkspaceID,
			ExecutionRoots: next.ExecutionRoots,
			// Files the person attached when the work was accepted. They were
			// imported into the person's partition before the row was written, so
			// they are managed copies and the turn sees exactly what was submitted.
			Attachments: attachmentsFromRefs(next.Attachments),
			// A system-originated finalization row carries the task it closes;
			// resolveTask honors an explicit TaskID before any label guess.
			TaskID: next.TaskID,
			// Reply metadata survives the durable queue so a threaded reply that
			// waited behind an active run still binds its exact parent run.
			ReplyToRunID: next.ReplyToRunID,
			ApprovalID:   next.ApprovalID,
			ClarifyID:    next.ClarifyID,
			Async:        true,
			// Carry the queue row id so the drained run's finalization marks it done
			// (QueueStatusDone) — otherwise the row stays 'started' and boot recovery
			// re-runs the already-completed work.
			QueueID:         next.ID,
			QueueClaimToken: claimToken,
			EffectKey:       next.IdempotencyKey,
		}
		req = c.srv.restoreQueuedAuthority(ctx, req, *next)
		// Reproduce the queued item's route while preserving its durable person.
		// Never create an account here: system rows may intentionally omit
		// platform_user_id, and normalizing that blank value to cli:local used to
		// move external-watch finalization onto a different person.
		drainIdentity := c.srv.routeIdentityForPerson(ctx, next.TenantID, next.PersonID, next.Channel, next.Platform, identity)
		// A drained item is an ordinary agent-bound message: rules-based intent
		// only, and resolveTask gives it the same pre-label guess as any other
		// message (Work Timeline P3) — harmless, since labels never gate context.
		intent := c.srv.classifyIntent(ctx, req.Content, req.Channel)
		// A queued item never re-enqueues, so it has no queue write to refuse.
		resp, refusal := c.startAsyncRun(drainIdentity, req, intent)
		if !resp.Accepted {
			// A typed synchronous refusal means no worker or finalizer started.
			// Return its exact unbound claim for the next periodic drain, as for
			// a capacity race. Refusals from a started worker keep their finalizer;
			// requeueing those can race settlement and cause an immediate spin.
			if refusal != nil || (resp.Turn != nil && resp.Turn.Status == "capacity_wait") {
				if _, err := c.returnUnboundQueueClaim(ctx, next.TenantID, next.ID, claimToken); err != nil {
					log.Warn("gateway: release unbound queue claim failed", "queue_id", next.ID, "error", err)
				}
			}
			if refusal != nil {
				log.Warn("gateway: queued admission was refused before execution; input retained for a later drain", "queue_id", next.ID, "error", refusal.cause)
			}
			// A confirmed startup refusal holds only its own source for this
			// bounded pass. Other sessions can use the remaining capacity, even
			// when the failed row is still started because its return write failed.
			heldSources[queuedSourceKey(*next)] = true
			continue
		}
		startedRun = true
		return
	}
}

func queuedSourceKey(q control.QueuedTask) string {
	return q.Platform + "\x00" + q.PlatformUserID + "\x00" + q.Channel
}

// The queue uses the same physical path identity as the worker pool, but only
// for readiness. The worker still owns the final lock after admission. Unknown
// or missing roots are a person-level conflict until their scope is known.
func queuedResourcesReady(q control.QueuedTask, active []*activeRun, durable ...[]control.RunRootReservation) bool {
	return queuedResourceBlocker(q, active, durable...) == nil
}

func queuedResourceBlocker(q control.QueuedTask, active []*activeRun, durable ...[]control.RunRootReservation) *queueResourceBlock {
	qPaths := writableRootPaths(q.ExecutionRoots)
	for _, reservations := range durable {
		for _, reserved := range reservations {
			active = append(active, &activeRun{RunID: reserved.RunID, ExecutionRoots: reserved.ExecutionRoots})
		}
	}
	for _, running := range active {
		if running == nil {
			continue
		}
		if q.ReplyToRunID != "" && q.ReplyToRunID == running.RunID {
			return &queueResourceBlock{Kind: "continuation", RunID: running.RunID}
		}
		if q.Platform == "cli" && running.Platform == "cli" && q.Channel != "" && q.Channel == running.Channel {
			return &queueResourceBlock{Kind: "session", RunID: running.RunID}
		}
		rPaths := writableRootPaths(running.ExecutionRoots)
		if len(qPaths) == 0 || len(rPaths) == 0 {
			return &queueResourceBlock{Kind: "unknown_scope", RunID: running.RunID}
		}
		if runpool.PathsConflict(qPaths, rPaths) &&
			(running.RunID == "" || !independentGitViewCandidate(q.ExecutionRoots, running.ExecutionRoots)) {
			return &queueResourceBlock{Kind: "directory", RunID: running.RunID}
		}
	}
	return nil
}

func independentGitViewCandidate(queued, active []executionenv.RootBinding) bool {
	if len(queued) != 1 || len(active) != 1 {
		return false
	}
	q, held := queued[0], active[0]
	return q.Source == executionenv.RootSourceWorkspace && held.Source == executionenv.RootSourceWorkspace &&
		q.Role == executionenv.RootRolePrimary && held.Role == executionenv.RootRolePrimary &&
		q.Writable() && held.Writable() && q.Path == held.Path && q.GitBaseline != nil && held.GitBaseline != nil && *q.GitBaseline == *held.GitBaseline
}

func writableRootPaths(bindings []executionenv.RootBinding) []string {
	paths := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Writable() && strings.TrimSpace(binding.Path) != "" {
			paths = append(paths, binding.Path)
		}
	}
	return paths
}
