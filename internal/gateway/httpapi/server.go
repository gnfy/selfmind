package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

type Server struct {
	Control           *control.Store
	Gateway           *router.Gateway
	Delivery          *delivery.Service
	DefaultTenantID   string
	DrainTimeout      time.Duration
	ShutdownFunc      func()
	RuntimeStatusFunc func() api.GatewayRuntimeInfo
	// ApprovalJudge is the optional cheap-model judge for smart-mode LLM approval
	// triage (H2). Installed on every execution scope so a dangerous
	// (non-hardline) op in smart mode is triaged before the human ask. Nil (e.g.
	// eval, or no cheap model) makes smart mode degrade to a human ask — never an
	// auto-approval.
	ApprovalJudge tools.ApprovalJudge

	mu           sync.Mutex
	draining     bool
	drainReason  string
	shutdownOnce sync.Once

	runs     *RunCoordinator
	runsOnce sync.Once

	// presence is the in-memory endpoint-attachment registry (presence.go).
	// Lazily built like the coordinator so every Server construction path
	// (HTTP handlers, IM adapters, eval harness, tests) gets one.
	presence     *presenceRegistry
	presenceOnce sync.Once
}

type activeRun struct {
	TenantID  string
	PersonID  string
	TaskID    string
	RunID     string
	Channel   string
	Summary   string
	StartedAt time.Time
	Cancel    context.CancelFunc
	// Steer carries mid-turn user guidance into the running agent loop
	// (installed on the run ctx via kernel.WithSteering; drained at iteration
	// boundaries). Buffered so /v1/runs/steer can hand off without blocking the
	// HTTP handler; a full buffer is reported as back-pressure, never dropped.
	Steer chan string
}

// steerBufferSize bounds queued-but-undrained guidance per run. Small on
// purpose: steering is corrective input, not a chat backlog; overflow surfaces
// as 429 so the client can tell the user instead of silently eating text.
const steerBufferSize = 4

func (d *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.handleHealth)
	mux.HandleFunc("/v1/message", d.handleMessage)
	mux.HandleFunc("/v1/dispatch", d.handleDispatch)
	mux.HandleFunc("/v1/im/", d.handleIMWebhook)
	mux.HandleFunc("/v1/accounts/bind", d.handleAccountBind)
	mux.HandleFunc("/v1/approvals", d.handleApprovals)
	mux.HandleFunc("/v1/approvals/respond", d.handleApprovalRespond)
	mux.HandleFunc("/v1/runs/steer", d.handleRunSteer)
	mux.HandleFunc("/v1/tasks/events", d.handleTaskEvents)
	mux.HandleFunc("/v1/tasks/events/stream", d.handleTaskEventsStream)
	mux.HandleFunc("/v1/tasks/artifacts", d.handleTaskArtifacts)
	mux.HandleFunc("/v1/tasks", d.handleTasks)
	mux.HandleFunc("/v1/tasks/current", d.handleCurrentTask)
	mux.HandleFunc("/v1/workspaces/register", d.handleWorkspaceRegister)
	mux.HandleFunc("/v1/workspaces", d.handleWorkspaces)
	mux.HandleFunc("/v1/gateway/status", d.handleGatewayStatus)
	mux.HandleFunc("/v1/gateway/shutdown", d.handleGatewayShutdown)
	mux.HandleFunc("/v1/presence/ping", d.handlePresencePing)
	mux.HandleFunc("/v1/digest", d.handleDigest)
	return mux
}

// presenceTracker lazily builds the per-Server presence registry.
func (d *Server) presenceTracker() *presenceRegistry {
	d.presenceOnce.Do(func() {
		d.presence = newPresenceRegistry(presenceTTL)
	})
	return d.presence
}

// touchPresence records an endpoint liveness beat for routing decisions and,
// throttled by the registry, refreshes the account's durable last_seen_at so
// preferred-endpoint selection tracks where the person actually is.
func (d *Server) touchPresence(ctx context.Context, identity *control.IdentityContext) {
	if d == nil || identity == nil {
		return
	}
	if d.presenceTracker().Touch(identity.PersonID, identity.Platform) && d.Control != nil {
		_ = d.Control.TouchAccountLastSeen(ctx, identity.TenantID, identity.AccountID)
	}
}

// handlePresencePing is the lightweight attachment heartbeat for idle
// interactive clients (the TUI pings every 30s while open). It resolves
// identity exactly like the other endpoints and only touches derived presence
// state — it never mutates tasks, runs, or approvals.
func (d *Server) handlePresencePing(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	d.touchPresence(r.Context(), identity)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	resp, status := d.ProcessMessage(r.Context(), req)
	writeJSON(w, status, resp)
}

func (d *Server) ProcessMessage(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
	req.TenantID = d.tenantID(req.TenantID)
	req.Platform = fallback(req.Platform, "cli")
	req.PlatformUserID = fallback(req.PlatformUserID, "local")
	req.Channel = fallback(req.Channel, req.Platform)
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		return api.MessageResponse{Error: "content is required", Turn: messageTurn("failed", "", "", "", "", "content is required")}, http.StatusBadRequest
	}

	identity, err := d.Control.ResolveOrCreateAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return api.MessageResponse{Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
	}
	// Any inbound message is a presence beat for its endpoint: a CLI turn
	// marks the terminal attached, an IM message refreshes that account's
	// recency for preferred-endpoint selection.
	d.touchPresence(ctx, identity)

	if handled, content, err := d.tryHandleControlCommand(ctx, identity, req); handled {
		if err != nil {
			return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
		}
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK
	}
	// A leading-slash message that no control command claimed is a mistyped
	// COMMAND, not conversation or new work. Reject it here so it can never be
	// dispatched to the agent or queued behind a running task (observed live:
	// "/qwer" created a task and joined the queue). Skill slash-invocation on
	// the gateway uses /v1/dispatch, not this message path, so rejecting every
	// unrecognized slash here is safe.
	if strings.HasPrefix(strings.TrimSpace(req.Content), "/") {
		msg := "Unknown command " + strings.Fields(strings.TrimSpace(req.Content))[0] + ". Send /help for the list of commands."
		if suggestion := suggestControlCommand(strings.ToLower(strings.TrimSpace(req.Content))); suggestion != "" {
			msg = "Unknown command " + strings.Fields(strings.TrimSpace(req.Content))[0] + " — did you mean " + suggestion + "? Send /help for the full list."
		}
		return api.MessageResponse{Identity: identity, Content: msg, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK
	}

	if d.IsDraining() {
		return api.MessageResponse{
			Identity: identity,
			Error:    "gateway is shutting down; try again after restart",
			Accepted: false,
			Turn:     messageTurn("failed", "", "draining", "", "", "gateway is shutting down"),
		}, http.StatusServiceUnavailable
	}

	intent := d.classifyIntent(ctx, req.Content, req.Channel)
	if intent.Intent != router.IntentContinue && looksLikeAffirmativeContinuation(req.Content) {
		if task, _ := d.resolveContinueTask(ctx, identity); task != nil {
			intent = router.IntentResult{
				Intent:           router.IntentContinue,
				Confidence:       0.84,
				Reason:           "affirmative reply with existing task context",
				Signals:          []string{"continue.affirmative_with_context"},
				ShouldCreateTask: true,
				ShouldUseTools:   true,
				Source:           "httpapi",
			}
		}
	}
	if handled, resp := d.tryHandleIntentClarification(identity, intent); handled {
		return resp, http.StatusOK
	}
	if handled, resp := d.tryHandleDirectIntent(ctx, identity, req, intent); handled {
		return resp, http.StatusOK
	}

	coord := d.coordinator()
	if running := coord.currentActive(identity.PersonID); running != nil {
		// A continuation steers/continues the ACTIVE task, so it is not new work
		// and must never be queued — keep the honest busy reply (steering proper
		// goes through /v1/runs/steer). Everything else is genuinely new work:
		// enqueue it and auto-start when the active run finishes (G1+G2), instead
		// of rejecting it as "busy" (which killed 24/7 IM dispatch).
		if intent.Intent == router.IntentContinue {
			return api.MessageResponse{
				Identity: identity,
				Content:  formatBusyRun(running),
				Accepted: false,
				Turn:     messageTurn("busy", "running", "running", running.TaskID, running.RunID, running.Summary),
			}, http.StatusOK
		}
		return d.enqueueBehindActive(ctx, identity, req), http.StatusOK
	}

	if req.Async {
		return coord.startAsyncRun(identity, req, intent), http.StatusOK
	}
	// Run lifetime is daemon-owned; endpoints detach, never cancel
	// (docs/identity-continuity.md "Runtime attachment model"). The run ctx is
	// derived WithoutCancel from the request ctx, so a client that closes its
	// terminal or drops the HTTP connection mid-turn detaches a watcher instead
	// of killing the run, while ctx values (stream observer, steering,
	// workspace scope) stay attached. A caller-supplied deadline is a bound on
	// the RUN (eval turn budgets, client turn timeouts), not on the
	// connection, so it is re-applied. Cancellation ownership lives in the
	// active-run registry: /stop, gateway drain (stopAllActive), and the TUI's
	// ctrl+c (which now routes through /stop) cancel via activeRun.Cancel; the
	// idle watchdog derives its own cancellable ctx inside RunAgentWithEvents.
	runCtx := context.WithoutCancel(ctx)
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		runCtx, cancel = context.WithDeadline(runCtx, deadline)
	} else {
		runCtx, cancel = context.WithCancel(runCtx)
	}
	defer cancel()
	// The steering channel is registered on the active run (so /v1/runs/steer
	// can reach it) AND installed on the run ctx (so the agent loop drains it).
	steerCh := make(chan string, steerBufferSize)
	if ok := coord.beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		Channel:   req.Channel,
		Summary:   truncate(req.Content, 240),
		StartedAt: time.Now(),
		Cancel:    cancel,
		Steer:     steerCh,
	}); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop.", Turn: messageTurn("busy", "running", "running", "", "", "")}, http.StatusOK
	}
	// Free the per-person slot, then drain the queue: a queued item auto-starts
	// as an async run once this sync run is done. drainQueue runs after endActive
	// (defers are LIFO) and only launches when no run raced back in.
	defer func() {
		coord.endActive(identity.PersonID)
		coord.drainQueue(identity)
	}()
	runCtx = kernel.WithSteering(runCtx, steerCh)

	resp, status := coord.runMessage(runCtx, identity, req, intent)
	// Detached finish: if the endpoint that dispatched this sync turn vanished
	// mid-run (the ORIGINAL request ctx was cancelled — not deadline-expired,
	// and the run itself was not cancelled), nobody reads the HTTP response,
	// so route the result like an async one: IM fan-out for cli-originated
	// runs, the source channel otherwise. A client that stayed connected gets
	// the sync answer only, and a user-cancelled run pushes nothing.
	if ctx.Err() != nil && context.Cause(ctx) == context.Canceled && !turnCancelled(resp) {
		coord.deliverAsyncResult(context.Background(), identity, req, resp)
	}
	return resp, status
}

// turnCancelled reports whether the turn ended because the run was cancelled
// (user /stop, drain) — those must never trigger a detached-result push.
func turnCancelled(resp api.MessageResponse) bool {
	return resp.Turn != nil && resp.Turn.Status == "cancelled"
}

func (c *RunCoordinator) prepareRequestWorkspace(ctx context.Context, identity *control.IdentityContext, req *api.MessageRequest) (*control.Workspace, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || req == nil {
		return nil, nil
	}
	store := c.srv.Control
	if strings.TrimSpace(req.WorkspaceID) != "" {
		return store.GetWorkspace(ctx, identity.TenantID, req.WorkspaceID)
	}
	if !isLocalCLIRequest(*req) {
		return nil, nil
	}
	cwd := cleanClientCWD(req.ClientCWD)
	if cwd == "" {
		return nil, nil
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	ws, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          filepath.Base(cwd),
		LocalPath:     cwd,
		AllowedRoots:  []string{cwd},
	})
	if err != nil {
		return nil, err
	}
	if ws != nil {
		req.WorkspaceID = ws.ID
	}
	return ws, nil
}

func cleanClientCWD(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return ""
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return ""
		}
		clean = abs
	}
	return clean
}

func isLocalCLIRequest(req api.MessageRequest) bool {
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	switch platform {
	case "cli", "terminal", "tui":
		return true
	}
	switch channel {
	case "cli", "terminal", "tui":
		return true
	}
	return false
}

func messageTurn(status, taskStatus, backgroundStatus, taskID, runID, message string) *api.TurnStatus {
	return &api.TurnStatus{
		Status:           strings.TrimSpace(status),
		TaskStatus:       strings.TrimSpace(taskStatus),
		BackgroundStatus: strings.TrimSpace(backgroundStatus),
		TaskID:           strings.TrimSpace(taskID),
		RunID:            strings.TrimSpace(runID),
		Message:          strings.TrimSpace(message),
	}
}

func messageContextBudget(usage llm.UsageStats) *api.ContextBudgetInfo {
	budget := kernel.DefaultRuntimeContextBudget()
	return &api.ContextBudgetInfo{
		TotalChars:            budget.TotalChars,
		WorkspaceChars:        budget.WorkspaceChars,
		TaskChars:             budget.TaskChars,
		MemoryChars:           budget.MemoryChars,
		EstimatedInputTokens:  usage.InputTokens,
		EstimatedOutputTokens: usage.OutputTokens,
	}
}

func llmUsageZero() llm.UsageStats {
	return llm.UsageStats{}
}

func (c *RunCoordinator) recordOutcomeArtifacts(ctx context.Context, task *control.Task, run *control.Run, channel string, files []string) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return
	}
	store := c.srv.Control
	seen := map[string]struct{}{}
	for _, uri := range files {
		uri = strings.TrimSpace(uri)
		if uri == "" {
			continue
		}
		if _, ok := seen[uri]; ok {
			continue
		}
		seen[uri] = struct{}{}
		artifact := control.Artifact{
			TaskID: task.ID,
			Kind:   artifactKind(uri),
			Name:   artifactName(uri),
			URI:    uri,
			Metadata: mustJSON(map[string]interface{}{
				"source": "run_outcome",
			}),
		}
		if run != nil {
			artifact.RunID = run.ID
		}
		saved, err := store.SaveArtifact(ctx, artifact)
		if err != nil {
			continue
		}
		runID := ""
		if run != nil {
			runID = run.ID
		}
		_, _ = store.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			RunID:      runID,
			Type:       "artifact.created",
			Visibility: "task",
			Channel:    channel,
			Payload: mustJSON(map[string]interface{}{
				"artifact": saved,
			}),
		})
	}
}

func artifactKind(uri string) string {
	lower := strings.ToLower(strings.TrimSpace(uri))
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "link"
	}
	return "file"
}

func artifactName(uri string) string {
	clean := strings.TrimSpace(uri)
	if clean == "" {
		return ""
	}
	if strings.Contains(clean, "://") {
		parts := strings.Split(strings.TrimRight(clean, "/"), "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
		return clean
	}
	name := filepath.Base(filepath.FromSlash(clean))
	if name == "." || name == string(filepath.Separator) {
		return clean
	}
	return name
}

// resolveTask decides which task this turn runs under. Attach happens ONLY on
// explicit continuation evidence: a caller-supplied task id, an IntentContinue
// classification (router cue or the short-acceptance upgrade in
// ProcessMessage), or the one-shot pin written by /resume. Everything else
// that reaches the agent is genuinely NEW work — async dispatches and
// queued-task drains included — and always gets its OWN task, even when a
// parked non-terminal task exists. Attaching new work to a parked task made
// the run inherit that task's title/status and (absent or wrong) workspace
// (observed live: an async "create docsite-demo" request landing on an
// unrelated /new-created empty task, so every file op tripped out-of-root
// approvals). Parked tasks stay reachable via /resume or a later continuation
// cue (resolveContinueTask); see docs/identity-continuity.md.
func (c *RunCoordinator) resolveTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (*control.Task, error) {
	store := c.srv.Control
	if req.TaskID != "" {
		task, err := store.GetTask(ctx, identity.TenantID, req.TaskID)
		if err != nil || task != nil {
			return c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
		}
	}
	// The /resume pin covers exactly the NEXT agent-bound message, so it is
	// consumed here no matter which branch wins below — a stale pin must never
	// capture unrelated work later.
	pinned := c.srv.consumeResumePin(ctx, identity)
	if intent.Intent == router.IntentContinue {
		task, err := c.srv.resolveContinueTask(ctx, identity)
		if err != nil || task != nil {
			return c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
		}
		return nil, fmt.Errorf("no task to continue; start a new task or use /resume <task_id>")
	}
	if pinned != nil && !terminalTaskStatus(pinned.Status) {
		return c.bindTaskWorkspaceIfMissing(ctx, identity, pinned, req, nil)
	}
	// No continuation evidence → new task. An explicit request workspace wins;
	// otherwise the person's current workspace seeds the task scope.
	workspaceID := req.WorkspaceID
	if workspaceID == "" {
		if ws, _ := store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); ws != nil {
			workspaceID = ws.ID
		}
	}
	return store.CreateTask(ctx, control.TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: workspaceID,
		Title:       titleFromInput(req.Content),
		Channel:     req.Channel,
	})
}

func (c *RunCoordinator) bindTaskWorkspaceIfMissing(ctx context.Context, identity *control.IdentityContext, task *control.Task, req api.MessageRequest, priorErr error) (*control.Task, error) {
	if priorErr != nil || task == nil || identity == nil {
		return task, priorErr
	}
	if task.WorkspaceID != "" || strings.TrimSpace(req.WorkspaceID) == "" {
		return task, nil
	}
	store := c.srv.Control
	if err := store.SetTaskWorkspace(ctx, identity.TenantID, task.ID, req.WorkspaceID); err != nil {
		return nil, err
	}
	refreshed, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || refreshed == nil {
		return task, err
	}
	return refreshed, nil
}

func (c *RunCoordinator) workspaceForTask(ctx context.Context, identity *control.IdentityContext, task *control.Task, req api.MessageRequest) (*control.Workspace, error) {
	store := c.srv.Control
	workspaceID := req.WorkspaceID
	if workspaceID == "" && task != nil {
		workspaceID = task.WorkspaceID
	}
	if workspaceID == "" {
		return store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	}
	return store.GetWorkspace(ctx, identity.TenantID, workspaceID)
}

func (c *RunCoordinator) installExecutionScope(identity *control.IdentityContext, task *control.Task, run *control.Run, workspace *control.Workspace, req api.MessageRequest) func() {
	if identity == nil {
		return func() {}
	}
	scope := tools.ExecutionScope{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
	}
	if workspace != nil {
		scope.WorkspaceID = workspace.ID
		scope.WorkspaceRoot = workspace.LocalPath
		scope.AllowedRoots = workspace.AllowedRoots
	}
	if task != nil {
		scope.TaskID = task.ID
	}
	if run != nil {
		scope.RunID = run.ID
		scope.Channel = run.Channel
	}
	scope.Approval = c.toolApprovalHandler(identity, task, run, scope.Channel)
	scope.Clarify = c.gatewayClarify(identity, task, run, scope.Channel)
	scope.ApprovalMode = c.resolveApprovalMode(identity, req.ApprovalMode)
	// Grants back class-level approval memory (session/persistent allowlist);
	// the control store satisfies tools.ApprovalGrantStore structurally.
	if c.srv != nil && c.srv.Control != nil {
		scope.Grants = c.srv.Control
	}
	// Judge backs smart-mode LLM approval triage (H2). Optional: nil leaves smart
	// mode on the human-ask path (never auto-approves without a judge).
	if c.srv != nil {
		scope.Judge = c.srv.ApprovalJudge
	}
	return tools.SetExecutionScope(identity.PersonID, scope)
}

// resolveApprovalMode applies the approval-mode precedence: an explicit
// per-request mode wins; otherwise the person's persisted /mode preference
// (approval_mode); otherwise on-request. This is what makes an IM `/mode smart`
// apply to later messages that carry no mode of their own.
func (c *RunCoordinator) resolveApprovalMode(identity *control.IdentityContext, reqMode string) tools.ApprovalMode {
	modeStr := strings.TrimSpace(reqMode)
	if modeStr == "" && c != nil && c.srv != nil && c.srv.Control != nil && identity != nil {
		if pref, err := c.srv.Control.GetPersonSetting(context.Background(), identity.TenantID, identity.PersonID, personSettingApprovalMode); err == nil {
			modeStr = pref
		}
	}
	return tools.NormalizeApprovalMode(modeStr)
}

// clarifyFallbackSentinel is returned when a question goes unanswered (timeout
// or the run's ctx is done): the agent falls back to its own best judgment
// instead of hanging, so behavior stays safe even when nobody replies. It is
// the SAME instruction the old stub returned, so the unanswered path is
// unchanged from the model's perspective.
const clarifyFallbackSentinel = "No answer arrived in time (the user may be away). Do not wait any longer: proceed using your own best judgment, state the assumption you are making, and continue the task."

// clarifyWaitTimeout bounds how long a run blocks on a pending question — the
// same 30-minute bound approvals use.
const clarifyWaitTimeout = 30 * time.Minute

// gatewayClarify answers the clarify tool as a FIRST-CLASS pending question,
// modeled exactly on the approval waiter (toolApprovalHandler): it creates a
// durable clarify_requests row, appends the clarify.requested event, pushes a
// presence-aware notification to the person's endpoints, then blocks polling the
// DB row until an answer arrives or the wait bound / run ctx expires. An answer
// (recorded from ANY endpoint via AnswerClarifyRequest) is returned verbatim as
// the tool result; a timeout expires the row and returns the best-judgment
// sentinel so the run never hangs. This is what lets a question survive the CLI
// closing (docs/identity-continuity.md "Runtime attachment model").
func (c *RunCoordinator) gatewayClarify(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ClarifyHandler {
	return func(question string, choices []string) string {
		if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
			return clarifyFallbackSentinel
		}
		store := c.srv.Control
		taskID, runID := "", ""
		if task != nil {
			taskID = task.ID
		}
		if run != nil {
			runID = run.ID
		}
		reqChannel := fallback(channel, identity.Platform)
		// The run ctx is not available here (the clarify handler signature carries
		// no ctx), so the wait is bounded purely by clarifyWaitTimeout; the
		// orphan sweep (ExpireOrphanedClarifies) is the backstop if the daemon is
		// killed mid-wait.
		waitCtx, cancel := context.WithTimeout(context.Background(), clarifyWaitTimeout)
		defer cancel()

		clarify, err := store.CreateClarifyRequest(waitCtx, control.ClarifyRequest{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			TaskID:   taskID,
			RunID:    runID,
			Question: question,
			Options:  mustJSON(choices),
			Channel:  reqChannel,
		})
		if err != nil {
			// Cannot durably record the question: fall back rather than hang.
			return clarifyFallbackSentinel
		}
		if taskID != "" {
			_, _ = store.AppendEvent(waitCtx, control.Event{
				TaskID:     taskID,
				RunID:      runID,
				Type:       "clarify.requested",
				Visibility: "task",
				Channel:    reqChannel,
				Payload:    mustJSON(map[string]interface{}{"clarify_id": clarify.ID, "question": question, "choices": choices}),
			})
		}
		c.notifyClarifyRequested(context.Background(), identity, taskID, runID, reqChannel, clarify)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				// The waiter is gone (timeout): a row left 'pending' would keep
				// intercepting the next free-text message as an answer to a
				// question nobody is waiting on. Expire it; the orphan sweep is
				// the backstop for waiters that die without reaching this line.
				_ = store.ExpireClarifyRequest(context.WithoutCancel(waitCtx), identity.TenantID, clarify.ID, "waiter gone: "+waitCtx.Err().Error())
				return clarifyFallbackSentinel
			case <-ticker.C:
				current, err := store.GetClarifyRequest(waitCtx, identity.TenantID, clarify.ID)
				if err != nil || current == nil {
					// Store hiccup / row vanished: don't hang, fall back.
					return clarifyFallbackSentinel
				}
				switch current.Status {
				case "answered":
					if answer := strings.TrimSpace(current.Answer); answer != "" {
						return answer
					}
					return clarifyFallbackSentinel
				case "expired":
					return clarifyFallbackSentinel
				}
			}
		}
	}
}

func (c *RunCoordinator) toolApprovalHandler(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ToolApprovalHandler {
	return func(ctx context.Context, req tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
		if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
			return tools.ToolApprovalDecision{}, fmt.Errorf("approval store is not available")
		}
		store := c.srv.Control
		if ctx == nil {
			ctx = context.Background()
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()

		taskID := req.TaskID
		runID := req.RunID
		if taskID == "" && task != nil {
			taskID = task.ID
		}
		if runID == "" && run != nil {
			runID = run.ID
		}
		approval, err := store.CreateApprovalRequest(waitCtx, control.ApprovalRequest{
			TenantID:         identity.TenantID,
			PersonID:         identity.PersonID,
			TaskID:           taskID,
			RunID:            runID,
			ActionType:       "tool_call",
			RequestedChannel: fallback(channel, identity.Platform),
			Payload: mustJSON(map[string]interface{}{
				"tool":   req.ToolName,
				"reason": req.Reason,
				"args":   redactApprovalArgs(req.Args),
			}),
		})
		if err != nil {
			return tools.ToolApprovalDecision{}, err
		}
		if taskID != "" {
			_, _ = store.AppendEvent(waitCtx, control.Event{
				TaskID:     taskID,
				RunID:      runID,
				Type:       "approval.requested",
				Visibility: "task",
				Channel:    fallback(channel, identity.Platform),
				Payload: mustJSON(map[string]interface{}{
					"approval_id": approval.ID,
					"action_type": approval.ActionType,
					"tool":        req.ToolName,
					"reason":      req.Reason,
				}),
			})
		}
		c.notifyApprovalRequested(context.Background(), identity, taskID, runID, fallback(channel, identity.Platform), approval)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				// The waiter is gone (timeout / run cancelled): a row left
				// 'pending' would pollute every later list — bare y turns
				// ambiguous and /approve 1 hits a dead request (observed
				// live). Expire it; the recovery sweep is the backstop for
				// waiters that die without reaching this line.
				_ = store.ExpireApprovalRequest(context.WithoutCancel(waitCtx), identity.TenantID, approval.ID, "waiter gone: "+waitCtx.Err().Error())
				return tools.ToolApprovalDecision{ApprovalID: approval.ID, Reason: waitCtx.Err().Error()}, waitCtx.Err()
			case <-ticker.C:
				current, err := store.GetApprovalRequest(waitCtx, identity.TenantID, approval.ID)
				if err != nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, err
				}
				if current == nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, fmt.Errorf("approval request disappeared: %s", approval.ID)
				}
				switch current.Status {
				case "approved":
					// DecisionScope (set when the human answered with a grant
					// scope) tells the middleware whether to remember this class
					// for the task/person.
					return tools.ToolApprovalDecision{Approved: true, ApprovalID: approval.ID, Scope: current.DecisionScope}, nil
				case "rejected":
					return tools.ToolApprovalDecision{Approved: false, ApprovalID: approval.ID, Reason: "rejected"}, nil
				}
			}
		}
	}
}

func redactApprovalArgs(args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	for k, v := range args {
		out[k] = tools.RedactSensitive(fmt.Sprintf("%v", v))
	}
	return out
}

// notifyApprovalRequested pushes an approval notification along the
// conversation-layer routing rules (docs/identity-continuity.md "Runtime
// attachment model"): IM-originated approvals notify their own channel
// (origin affinity); CLI-originated approvals are suppressed while a CLI
// endpoint is attached (the TUI already shows the inline y/N prompt — pushing
// to IM too was the double-notification bug) and otherwise go to the SINGLE
// preferred IM endpoint, never a fan-out to every bound account. Failures are
// swallowed: notification is a convenience, the approval row is the truth.
func (c *RunCoordinator) notifyApprovalRequested(ctx context.Context, identity *control.IdentityContext, taskID, runID, channel string, approval *control.ApprovalRequest) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil || approval == nil {
		return
	}
	taskTitle := ""
	if taskID != "" && c.srv.Control != nil {
		if task, err := c.srv.Control.GetTask(ctx, identity.TenantID, taskID); err == nil && task != nil {
			taskTitle = task.Title
		}
	}
	content := approvalNotificationText(*approval, taskTitle)
	base := delivery.Message{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     taskID,
		RunID:      runID,
		Content:    content,
		Kind:       delivery.KindApproval,
		ApprovalID: approval.ID,
	}
	c.routePendingNotification(ctx, identity, channel, base)
}

// notifyClarifyRequested pushes a pending-question notification along the SAME
// conversation-layer routing rules as approvals (docs/identity-continuity.md
// "Runtime attachment model"): IM-originated questions notify their own channel;
// CLI-originated questions are suppressed while a CLI endpoint is attached (the
// live TUI already shows the clarify.requested event) and otherwise go to the
// SINGLE preferred IM endpoint. Failures are swallowed: the clarify row is the
// truth; the push is a convenience. A question survives the endpoint that raised
// it closing exactly like an approval does.
func (c *RunCoordinator) notifyClarifyRequested(ctx context.Context, identity *control.IdentityContext, taskID, runID, channel string, clarify *control.ClarifyRequest) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil || clarify == nil {
		return
	}
	base := delivery.Message{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   taskID,
		RunID:    runID,
		Content:  clarifyNotificationText(*clarify),
		Kind:     delivery.KindClarify,
	}
	c.routePendingNotification(ctx, identity, channel, base)
}

// routePendingNotification is the shared presence-aware, single-preferred-
// endpoint delivery used by BOTH approval and clarify pushes. Origin is CLI when
// the PLATFORM is cli — the channel is a session UUID for TUI turns and the
// literal "cli" only for `selfmind send`, so matching on channel would route
// TUI-originated pushes to a nonexistent "cli" sender. IM origin → the
// requesting channel only (no cross-channel duplication). CLI origin with a CLI
// endpoint attached → suppressed (the live TUI already renders the inline
// prompt/event; an IM push would be a duplicate). CLI origin detached → the one
// preferred IM endpoint, never a fan-out.
func (c *RunCoordinator) routePendingNotification(ctx context.Context, identity *control.IdentityContext, channel string, base delivery.Message) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil {
		return
	}
	if identity.Platform != "cli" {
		msg := base
		msg.Platform = identity.Platform
		msg.PlatformUserID = identity.PlatformUserID
		msg.Channel = channel
		_ = c.srv.Delivery.EnqueueAndTry(ctx, msg)
		return
	}
	if c.srv.presenceTracker().IsAttached(identity.PersonID, "cli") {
		return
	}
	c.deliverToPreferredIM(ctx, identity, base)
}

// personSettingNotifyPlatform is the person_settings key holding the explicit
// /notify endpoint preference; empty/absent means "auto" (most recently seen).
const personSettingNotifyPlatform = "notify_platform"

// personSettingApprovalMode is the person_settings key holding the person's
// persisted /mode (approval policy). Applied when a request carries no explicit
// per-request mode; empty/absent means on-request.
const personSettingApprovalMode = "approval_mode"

// preferredIMAccount picks the SINGLE IM endpoint for CLI-origin pushes when
// the CLI is detached (conversation-layer rule 4). Selection order:
//  1. The person's explicit /notify preference, when it still resolves to one
//     of their OWN bound, delivery-capable accounts (a stale preference —
//     unbound platform, sender removed — silently falls through to auto
//     rather than dropping the notification).
//  2. Auto: the most recently seen bound IM account the delivery service can
//     reach (accounts.last_seen_at, refreshed by inbound messages and
//     presence beats).
func (c *RunCoordinator) preferredIMAccount(ctx context.Context, identity *control.IdentityContext) *control.Account {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || c.srv.Control == nil || identity == nil {
		return nil
	}
	store := c.srv.Control
	supports := c.srv.Delivery.SupportsPlatform
	if pref, err := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform); err == nil && pref != "" && pref != "auto" {
		account, err := store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, func(platform string) bool {
			return platform == pref && supports(platform)
		})
		if err == nil && account != nil {
			return account
		}
	}
	account, err := store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, supports)
	if err != nil {
		return nil
	}
	return account
}

// deliverToPreferredIM delivers a CLI-origin message to exactly one endpoint —
// the preferred IM account — never a broadcast to every bound account
// (docs/identity-continuity.md: "One target, never a fan-out"). Shared by
// approval notifications and CLI-originated async/detached results.
func (c *RunCoordinator) deliverToPreferredIM(ctx context.Context, identity *control.IdentityContext, base delivery.Message) {
	account := c.preferredIMAccount(ctx, identity)
	if account == nil {
		return
	}
	msg := base
	msg.Platform = account.Platform
	msg.PlatformUserID = account.PlatformUserID
	// Without a live chat context the platform user id is the DM target;
	// senders fall back to it when Channel is not a real chat id.
	msg.Channel = account.PlatformUserID
	_ = c.srv.Delivery.EnqueueAndTry(ctx, msg)
}

// approvalNotificationText is the outbound approval message body: the same
// rich summary line the /approvals list shows, the copyable id, and bilingual
// reply instructions. Telegram additionally renders native buttons from
// Message.Kind/ApprovalID.
func approvalNotificationText(approval control.ApprovalRequest, taskTitle string) string {
	// Conversational, task-free: in IM the person just needs to answer "can I
	// run this?", like asking a human assistant. No task label, no apr_ id, no
	// ordinal — a bare y/n resolves to this approval (the only pending one on
	// the serial interactive path). The task concept stays in the control
	// plane, out of the IM UX. /approve <n> remains available for the rare
	// parallel-run case; /approvals still lists ids.
	return "Approval needed — reply y or n:\n" + approvalSummaryLine(approval, "")
}

// clarifyNotificationText is the outbound pending-question body: the question,
// its options (when any) as a short numbered list, and one instruction to just
// reply with the answer. Conversational and task-free like the approval push —
// the next non-command reply resolves it (docs/identity-continuity.md).
func clarifyNotificationText(clarify control.ClarifyRequest) string {
	var sb strings.Builder
	sb.WriteString("Question — reply with your answer:\n")
	sb.WriteString(strings.TrimSpace(clarify.Question))
	for i, option := range clarifyOptions(clarify) {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, option)
	}
	return sb.String()
}

// clarifyOptions decodes the options_json array, tolerating a null/empty/broken
// payload by returning no options (the question still stands on its own).
func clarifyOptions(clarify control.ClarifyRequest) []string {
	if len(clarify.Options) == 0 {
		return nil
	}
	var options []string
	if err := json.Unmarshal(clarify.Options, &options); err != nil {
		return nil
	}
	return options
}

func (c *RunCoordinator) withGatewayContext(input string, identity *control.IdentityContext, task *control.Task, workspace *control.Workspace, attachments []api.MessageAttachment) string {
	if (workspace == nil || workspace.LocalPath == "" || task == nil) && len(attachments) == 0 {
		return input
	}
	var sb strings.Builder
	sb.WriteString("[SelfMind daemon context]\n")
	if identity != nil {
		fmt.Fprintf(&sb, "person_id: %s\n", identity.PersonID)
		fmt.Fprintf(&sb, "platform: %s\n", identity.Platform)
		fmt.Fprintf(&sb, "platform_user_id: %s\n", identity.PlatformUserID)
	}
	if task != nil {
		fmt.Fprintf(&sb, "task_id: %s\n", task.ID)
	}
	if workspace != nil && workspace.LocalPath != "" {
		fmt.Fprintf(&sb, "workspace_id: %s\n", workspace.ID)
		fmt.Fprintf(&sb, "workspace_root: %s\n", workspace.LocalPath)
		sb.WriteString("Use workspace_root as the default cwd for local file tools.\n")
		sb.WriteString("When the user says current project, this repo, this codebase, or names a project without an explicit path, inspect workspace_root first.\n")
		sb.WriteString("Resolve relative paths against workspace_root. Do not access files outside workspace allowed roots.\n")
	}
	if len(attachments) > 0 {
		sb.WriteString("attachments:\n")
		for i, att := range attachments {
			fmt.Fprintf(&sb, "- index: %d\n", i+1)
			if att.Kind != "" {
				fmt.Fprintf(&sb, "  kind: %s\n", att.Kind)
			}
			if att.Path != "" {
				fmt.Fprintf(&sb, "  path: %s\n", att.Path)
			}
			if att.MimeType != "" {
				fmt.Fprintf(&sb, "  mime_type: %s\n", att.MimeType)
			}
			if att.Name != "" {
				fmt.Fprintf(&sb, "  name: %s\n", att.Name)
			}
			if att.Size > 0 {
				fmt.Fprintf(&sb, "  size: %d\n", att.Size)
			}
		}
		sb.WriteString("When useful, inspect attachment paths with local tools before answering.\n")
	}
	sb.WriteString("[/SelfMind daemon context]\n\n")
	sb.WriteString(input)
	return sb.String()
}

func (d *Server) tryHandleControlCommand(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (bool, string, error) {
	trimmed := strings.TrimSpace(req.Content)
	lower := strings.ToLower(trimmed)
	// Conversational approval: a bare "y"/"n" (or 好/可以/不行 …) answers a
	// pending approval without the /approve ceremony, so IM feels like asking
	// a human assistant. Only claimed when an approval is actually pending —
	// otherwise the word falls through to the agent (and to the continuation
	// cue handling for "ok"/"可以"). Runs before the "/" gate below.
	if handled, reply, err := d.tryHandleBareApprovalReply(ctx, identity, trimmed, req.Channel); handled {
		return true, reply, err
	}
	// Pending question: a plain (non-slash) reply while a clarify_requests row is
	// pending IS the answer (G3) — resolve it here, above the new-task/queue
	// logic, so a blocking run gets its answer instead of the reply being queued
	// or steered. Runs after the bare y/n approval leg (which wins for y/n-looking
	// input) and before the "/" gate (slash commands are never answers).
	if handled, reply, err := d.tryHandleClarifyAnswer(ctx, identity, trimmed, req.Channel); handled {
		return true, reply, err
	}
	if !strings.HasPrefix(lower, "/") {
		return false, "", nil
	}
	switch {
	case lower == "/help":
		// Canonical gateway help comes from the shared command registry so the
		// help text, the switch below, and every other endpoint cannot drift.
		return true, command.HelpText(), nil
	case lower == "/model":
		if d != nil && d.Gateway != nil {
			return true, d.Gateway.ModelStatusReply(), nil
		}
		return true, "SelfMind is running, but the model gateway is not configured.", nil
	case lower == "/id":
		return true, formatIdentity(identity), nil
	case lower == "/stop":
		active := d.coordinator().stopActive(identity.PersonID)
		if active == nil {
			// No live run — but a task can be stuck non-terminal
			// (in_progress/blocked/running) with no run behind it (e.g. a run
			// that finalized without terminalizing its task, or a task created
			// but never executed). /stop should still let the user terminate
			// it, otherwise it sits in /tasks forever with no way to clear it.
			return true, d.cancelStuckCurrentTask(ctx, identity), nil
		}
		if active.RunID != "" {
			_ = d.Control.RequestRunCancel(context.Background(), identity.TenantID, active.RunID)
			_ = d.Control.FinishRun(context.Background(), identity.TenantID, active.RunID, "cancelled")
		}
		if active.TaskID != "" {
			_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, active.TaskID, "cancelled", "Cancelled by user.", nil)
			_, _ = d.Control.AppendEvent(context.Background(), control.Event{
				TaskID:     active.TaskID,
				RunID:      active.RunID,
				Type:       "run.cancelled",
				Visibility: "task",
				Channel:    req.Channel,
				Payload:    mustJSON(map[string]string{"reason": "user requested stop"}),
			})
		}
		return true, fmt.Sprintf("Stopping run %s.", fallback(active.RunID, "(starting)")), nil
	case strings.HasPrefix(lower, "/cancel"):
		// Explicit "terminate a stuck task" — same as /stop's no-run fallback,
		// but a clearer verb for a task that is parked rather than running.
		return true, d.cancelStuckCurrentTask(ctx, identity), nil
	case strings.HasPrefix(lower, "/new"):
		title := strings.TrimSpace(trimmed[len("/new"):])
		if title == "" {
			title = "New task"
		}
		workspaceID := req.WorkspaceID
		if workspaceID == "" {
			if ws, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); ws != nil {
				workspaceID = ws.ID
			}
		}
		task, err := d.Control.CreateTask(ctx, control.TaskCreate{
			TenantID:    identity.TenantID,
			PersonID:    identity.PersonID,
			WorkspaceID: workspaceID,
			Title:       title,
			Channel:     req.Channel,
		})
		if err != nil {
			return true, "", err
		}
		return true, fmt.Sprintf("Created task: %s (%s)", task.Title, task.ID), nil
	case lower == "/task status" || lower == "/status":
		reply, err := d.statusReply(ctx, identity)
		return true, reply, err
	case strings.HasPrefix(lower, "/resume ") || strings.HasPrefix(lower, "/task "):
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /resume <task_id>", nil
		}
		task, err := d.Control.GetTask(ctx, identity.TenantID, parts[1])
		if err != nil {
			return true, "", err
		}
		if task == nil || task.PersonID != identity.PersonID {
			return true, "Task not found.", nil
		}
		if err := d.Control.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
			return true, "", err
		}
		// One-shot pin: an explicit /resume IS continuation evidence, so the
		// next agent-bound message attaches to this task even without a
		// continuation cue. Consumed by resolveTask; after that, plain new
		// messages create their own tasks again (task-attach semantics).
		_ = d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey, task.ID)
		return true, fmt.Sprintf("Resumed task: %s (%s)", task.Title, task.ID), nil
	case lower == "/tasks" || lower == "tasks" || strings.Contains(lower, "任务列表"):
		tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		if err != nil {
			return true, "", err
		}
		return true, formatTasks(tasks), nil
	case lower == "/queue" || strings.HasPrefix(lower, "/queue "):
		arg := strings.TrimSpace(trimmed[len("/queue"):])
		if strings.EqualFold(arg, "clear") {
			n, err := d.Control.ClearQueued(ctx, identity.TenantID, identity.PersonID)
			if err != nil {
				return true, "", err
			}
			return true, fmt.Sprintf("Cleared %d queued task(s).", n), nil
		}
		queued, err := d.Control.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		if err != nil {
			return true, "", err
		}
		// `/queue drop <n>` removes one item by its list position (same
		// ordering as the /queue listing), so a single unwanted task can be
		// dropped without clearing the whole queue.
		if rest := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(arg), "drop")); arg != "" && strings.HasPrefix(strings.ToLower(arg), "drop") {
			n, convErr := strconv.Atoi(strings.TrimSpace(rest))
			if convErr != nil || n < 1 || n > len(queued) {
				return true, fmt.Sprintf("Usage: /queue drop <n> (1-%d). Run /queue to see positions.", len(queued)), nil
			}
			target := queued[n-1]
			if err := d.Control.MarkQueued(ctx, identity.TenantID, target.ID, control.QueueStatusCancelled); err != nil {
				return true, "", err
			}
			return true, "Dropped queued task: " + textutil.Truncate(toOneLine(target.Content), 60), nil
		}
		return true, formatQueue(queued), nil
	case lower == "/diag":
		reply, err := d.diagReply(ctx, identity)
		return true, reply, err
	case strings.HasPrefix(lower, "/workspace "):
		parts := strings.Fields(req.Content)
		if len(parts) < 2 {
			return true, "Usage: /workspace <workspace_id>", nil
		}
		ws, err := d.Control.GetWorkspace(ctx, identity.TenantID, parts[1])
		if err != nil {
			return true, "", err
		}
		if ws == nil || ws.OwnerPersonID != identity.PersonID {
			return true, "Workspace not found.", nil
		}
		if err := d.Control.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID); err != nil {
			return true, "", err
		}
		return true, fmt.Sprintf("Current workspace: %s (%s)\n%s", ws.Name, ws.ID, ws.LocalPath), nil
	case lower == "/workspaces":
		workspaces, err := d.Control.ListWorkspaces(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		return true, formatWorkspaces(workspaces), nil
	case lower == "/approvals":
		approvals, titles, err := d.pendingApprovalsForDisplay(ctx, identity)
		if err != nil {
			return true, "", err
		}
		return true, formatApprovals(approvals, titles), nil
	case lower == "/notify" || strings.HasPrefix(lower, "/notify "):
		reply, err := d.notifyPreferenceReply(ctx, identity, strings.TrimSpace(trimmed[len("/notify"):]))
		return true, reply, err
	case lower == "/mode" || strings.HasPrefix(lower, "/mode "):
		reply, err := d.approvalModeReply(ctx, identity, strings.TrimSpace(trimmed[len("/mode"):]))
		return true, reply, err
	case lower == "/approve" || strings.HasPrefix(lower, "/approve "):
		return true, d.respondApprovalCommand(ctx, identity, strings.TrimSpace(trimmed[len("/approve"):]), "approved", req.Channel), nil
	case lower == "/reject" || strings.HasPrefix(lower, "/reject "):
		return true, d.respondApprovalCommand(ctx, identity, strings.TrimSpace(trimmed[len("/reject"):]), "rejected", req.Channel), nil
	case lower == "/events":
		task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		if task == nil {
			return true, "No active task.", nil
		}
		events, err := d.Control.ListTaskEvents(ctx, task.ID, 20)
		if err != nil {
			return true, "", err
		}
		return true, formatEvents(events), nil
	case lower == "/task status" || lower == "task status" || lower == "/status" || lower == "status" || strings.Contains(lower, "进度"):
		reply, err := d.statusReply(ctx, identity)
		return true, reply, err
	default:
		// Near-miss typo help: "/approves" → suggest "/approvals". Only claim
		// the message when the token is close to a KNOWN control command —
		// unknown slashes may be skill invocations or agent input and must
		// keep flowing through unchanged.
		if suggestion := suggestControlCommand(lower); suggestion != "" {
			return true, fmt.Sprintf("Unknown command %s — did you mean %s?", strings.Fields(lower)[0], suggestion), nil
		}
		return false, "", nil
	}
}

// notifyPreferenceReply handles /notify: show, set, or reset the person's
// preferred notify endpoint for detached CLI-origin pushes. Setting a concrete
// platform is validated against the person's OWN bound accounts — this bound
// check is a security boundary (never an arbitrary push target), not a
// convenience (docs/identity-continuity.md conversation-layer rule 1).
func (d *Server) notifyPreferenceReply(ctx context.Context, identity *control.IdentityContext, arg string) (string, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		current, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform)
		if err != nil {
			return "", err
		}
		if current == "" {
			current = "auto (most recently active IM account)"
		}
		return "Notify preference: " + current + "\nUsage: /notify <platform|auto>", nil
	}
	if arg == "auto" {
		if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform, ""); err != nil {
			return "", err
		}
		return "Notify preference set to auto (most recently active IM account).", nil
	}
	accounts, err := d.Control.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "", err
	}
	var bound []string
	valid := false
	for _, account := range accounts {
		if account.Platform == "cli" {
			continue
		}
		bound = append(bound, account.Platform)
		if account.Platform == arg {
			valid = true
		}
	}
	if !valid {
		if len(bound) == 0 {
			return "You have no bound IM accounts yet, so there is nothing to notify. Bind an account first.", nil
		}
		return fmt.Sprintf("%s is not one of your bound IM accounts (bound: %s). Use /notify <platform|auto>.", arg, strings.Join(bound, ", ")), nil
	}
	if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform, arg); err != nil {
		return "", err
	}
	return "Notify preference set to " + arg + ".", nil
}

// approvalModeReply handles /mode from any channel: show the person's current
// approval mode, or persist a new one (per person via person_settings). The
// persisted mode is applied when a later request carries no explicit per-request
// mode (see installExecutionScope). full-auto gets a warning that the hard-floor
// safety deny set still applies — it is not blocked, only flagged.
func (d *Server) approvalModeReply(ctx context.Context, identity *control.IdentityContext, arg string) (string, error) {
	const usage = "Usage: /mode <on-request|read-only|auto-edit|full-auto|smart>"
	arg = strings.TrimSpace(arg)
	if arg == "" {
		current, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(current) == "" {
			current = string(tools.ApprovalOnRequest) + " (default)"
		}
		return "Approval mode: " + current + "\n" + usage, nil
	}
	// Reject unknown words instead of silently defaulting: a typo'd mode should
	// not quietly leave the person on on-request thinking they set full-auto.
	if tools.NormalizeApprovalMode(arg) == tools.ApprovalOnRequest && !tools.IsKnownApprovalModeWord(arg) {
		return "Unknown mode " + arg + ".\n" + usage, nil
	}
	mode := string(tools.NormalizeApprovalMode(arg))
	if err := d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, mode); err != nil {
		return "", err
	}
	reply := "Approval mode set to " + mode + "."
	if mode == string(tools.ApprovalFullAuto) {
		reply += " Note: the hard-floor safety limits still apply (filesystem-root deletes, disk formatting, host shutdown, and similar are always blocked)."
	}
	return reply, nil
}

// suggestControlCommand returns the closest control command when the first
// token is a near-miss, delegating to the shared registry so the gateway and
// the TUI make the SAME unknown-command decision.
func suggestControlCommand(lower string) string {
	return command.Suggest(lower)
}

// statusReply builds the /status card shared by every channel's control-command
// path. When the person has an active run, its task is what the user is
// waiting on, so report it first (an async run may attach to a task the
// current_task pointer has not caught up with yet); fall back to the
// per-person current_task pointer otherwise. The card format itself
// (formatTaskStatus, `Task:` / `Status:` markers) is a stable contract pinned
// by the continuity eval suite — change it there, not here.
// cancelStuckCurrentTask terminates the person's current task when it is stuck
// in a non-terminal state with no live run (the /stop no-run fallback and the
// /cancel command). It never touches a task that is already terminal. This is
// the user-facing escape hatch for a task that recovery sweeps missed (created
// but never run, or finalized without terminalizing).
func (d *Server) cancelStuckCurrentTask(ctx context.Context, identity *control.IdentityContext) string {
	if d == nil || d.Control == nil || identity == nil {
		return "No active run to stop."
	}
	task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || task == nil {
		return "No active run to stop, and no current task to cancel."
	}
	if terminalTaskStatus(task.Status) {
		return "No active run to stop; the current task is already " + task.Status + "."
	}
	if err := d.Control.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "cancelled", "Cancelled by user.", nil); err != nil {
		return "Could not cancel the task: " + err.Error()
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		Type:       "task.cancelled",
		Visibility: "task",
		Payload:    mustJSON(map[string]string{"reason": "user cancelled a stuck task"}),
	})
	return "No live run was executing, so I cancelled the current task: " + textutil.Truncate(toOneLine(task.Title), 60)
}

func (d *Server) statusReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	active := d.coordinator().currentActive(identity.PersonID)
	var task *control.Task
	if active != nil && active.TaskID != "" {
		// Best-effort lookup: a missing/errored row falls back to the pointer.
		task, _ = d.Control.GetTask(ctx, identity.TenantID, active.TaskID)
	}
	if task == nil {
		var err error
		task, err = d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return "", err
		}
	}
	if task == nil {
		return "No active task.", nil
	}
	handoff, _ := d.Control.LatestHandoff(ctx, task.ID)
	plan := d.latestPlanForTask(ctx, task.ID)
	card := formatTaskStatus(task, handoff, active, plan)
	// A run blocked on an approval looks "stuck" unless the card says the run
	// is waiting for the HUMAN (observed live: 15 minutes of staring at
	// "running" while an ls_r approval sat pending). Surface it with the same
	// conversational summary the push uses.
	if pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 5); err == nil && len(pending) > 0 {
		sortApprovalsForDisplay(pending)
		card += "\n⚠ Waiting for your approval — reply y or n:\n"
		titles := d.taskTitlesFor(ctx, identity.TenantID, pending)
		for i, approval := range pending {
			if len(pending) == 1 {
				card += approvalSummaryLine(approval, "") + "\n"
				break
			}
			card += fmt.Sprintf("%d. %s\n", i+1, approvalSummaryLine(approval, titles[approval.TaskID]))
		}
	}
	// A run blocked on a clarify looks just as "stuck" as one blocked on an
	// approval: surface the pending question(s) so the person knows their reply
	// is what unblocks the run. ListClarifyRequests is already oldest-first, the
	// order tryHandleClarifyAnswer answers in.
	if clarifies, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 5); err == nil && len(clarifies) > 0 {
		card += "\n⚠ Waiting for your answer — just reply with it:\n"
		for i, clarify := range clarifies {
			if len(clarifies) == 1 {
				card += clarifySummaryLine(clarify) + "\n"
				break
			}
			card += fmt.Sprintf("%d. %s\n", i+1, clarifySummaryLine(clarify))
		}
	}
	return card, nil
}

// clarifySummaryLine renders a pending question as one compact line for /status,
// /diag, and the digest: the question, trimmed to one line and bounded.
func clarifySummaryLine(clarify control.ClarifyRequest) string {
	return truncate(toOneLine(clarify.Question), 160)
}

func (d *Server) identityFromQuery(r *http.Request) (*control.IdentityContext, error) {
	q := r.URL.Query()
	return d.Control.ResolveOrCreateAccount(
		r.Context(),
		d.tenantID(q.Get("tenant_id")),
		fallback(q.Get("platform"), "cli"),
		fallback(q.Get("platform_user_id"), "local"),
		q.Get("display_name"),
	)
}

func (d *Server) tenantID(tenantID string) string {
	return fallback(tenantID, fallback(d.DefaultTenantID, control.DefaultTenantID))
}

func (d *Server) authorized(r *http.Request) bool {
	token := gatewayToken()
	if token == "" {
		return true
	}
	if got := strings.TrimSpace(r.Header.Get("X-SelfMind-Token")); got == token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= len("Bearer ") && strings.EqualFold(auth[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):]) == token
	}
	return false
}

func gatewayToken() string {
	if token := strings.TrimSpace(os.Getenv("SELF_GATEWAY_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("SELF_DAEMON_TOKEN"))
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func mustJSON(value interface{}) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func fallback(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return textutil.TruncateBytes(value, max) + "..."
}

func titleFromInput(input string) string {
	title := strings.TrimSpace(input)
	if title == "" {
		return "Untitled task"
	}
	return truncate(title, 80)
}

func inferTaskStatus(content string) string {
	if looksTaskBlocked(content) {
		return "blocked"
	}
	if looksTaskComplete(content) {
		return "done"
	}
	return "running"

}

func looksTaskBlocked(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	return containsAny(lower, []string{
		"blocked",
		"cannot proceed",
		"can't proceed",
		"need your input",
		"needs your input",
		"waiting for you",
		"requires approval",
	}) || containsAny(content, []string{
		"\u963b\u585e",
		"\u65e0\u6cd5\u7ee7\u7eed",
		"\u9700\u8981\u4f60\u63d0\u4f9b",
		"\u9700\u8981\u4f60\u786e\u8ba4",
	})
}

func looksTaskComplete(content string) bool {
	trimmed := strings.TrimSpace(content)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return false
	}
	if containsAny(lower, []string{
		"not done",
		"not completed",
		"not finished",
		"remaining work",
		"still need",
		"need to continue",
		"next steps",
		"todo:",
	}) || containsAny(trimmed, []string{
		"\u672a\u5b8c\u6210",
		"\u8fd8\u6ca1\u5b8c\u6210",
		"\u6ca1\u6709\u5b8c\u6210",
		"\u5f85\u5b8c\u6210",
		"\u9700\u8981\u7ee7\u7eed",
	}) {
		return false
	}
	if lower == "done" || lower == "completed" || lower == "finished" || lower == "all done" {
		return true
	}
	return containsAny(lower, []string{
		"task complete",
		"task completed",
		"completed successfully",
		"finished successfully",
		"all done",
		"implementation complete",
		"tests pass",
	}) || containsAny(trimmed, []string{
		"\u5df2\u5b8c\u6210",
		"\u4efb\u52a1\u5b8c\u6210",
		"\u5904\u7406\u5b8c\u6210",
		"\u5df2\u5904\u7406\u5b8c",
		"\u5df2\u7ecf\u5b8c\u6210",
		"\u641e\u5b9a",
	})
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
