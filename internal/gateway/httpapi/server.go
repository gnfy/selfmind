package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
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

	mu           sync.Mutex
	draining     bool
	drainReason  string
	shutdownOnce sync.Once

	runs     *RunCoordinator
	runsOnce sync.Once
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
	return mux
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

	if handled, content, err := d.tryHandleControlCommand(ctx, identity, req); handled {
		if err != nil {
			return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
		}
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK
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
		return api.MessageResponse{
			Identity: identity,
			Content:  formatBusyRun(running),
			Accepted: false,
			Turn:     messageTurn("busy", "running", "running", running.TaskID, running.RunID, running.Summary),
		}, http.StatusOK
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
	defer coord.endActive(identity.PersonID)
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

func (c *RunCoordinator) resolveTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (*control.Task, error) {
	store := c.srv.Control
	if req.TaskID != "" {
		task, err := store.GetTask(ctx, identity.TenantID, req.TaskID)
		if err != nil || task != nil {
			return c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
		}
	}
	if intent.Intent == router.IntentContinue {
		task, err := c.srv.resolveContinueTask(ctx, identity)
		if err != nil || task != nil {
			return c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
		}
		return nil, fmt.Errorf("no task to continue; start a new task or use /resume <task_id>")
	}
	// Channel-scoped so two concurrent sessions (e.g. two CLI terminals) don't
	// share one "current task" and bleed context into each other.
	task, err := store.CurrentTaskForChannel(ctx, identity.TenantID, identity.PersonID, req.Channel)
	if err != nil {
		return nil, err
	}
	if task != nil {
		return c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, nil)
	}
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
	scope.ApprovalMode = tools.NormalizeApprovalMode(req.ApprovalMode)
	return tools.SetExecutionScope(identity.PersonID, scope)
}

// gatewayClarify answers the clarify tool in non-interactive (gateway/IM)
// contexts. There is no blocking prompt channel here, so instead of hanging it
// records a clarify.requested event and returns a sentinel telling the agent to
// present the question and end the turn; the user's reply arrives as a normal
// follow-up message that continues the task.
func (c *RunCoordinator) gatewayClarify(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ClarifyHandler {
	return func(question string, choices []string) string {
		taskID, runID := "", ""
		if task != nil {
			taskID = task.ID
		}
		if run != nil {
			runID = run.ID
		}
		if c != nil && c.srv != nil && c.srv.Control != nil && taskID != "" {
			_, _ = c.srv.Control.AppendEvent(context.Background(), control.Event{
				TaskID:     taskID,
				RunID:      runID,
				Type:       "clarify.requested",
				Visibility: "task",
				Channel:    channel,
				Payload:    mustJSON(map[string]interface{}{"question": question, "choices": choices}),
			})
		}
		var sb strings.Builder
		sb.WriteString("This is a non-interactive channel, so there is no live prompt. ")
		sb.WriteString("End your turn now and ask the user this question as your reply")
		if len(choices) > 0 {
			sb.WriteString(" (present the options as a short numbered list)")
		}
		sb.WriteString(". They will answer in a follow-up message that continues this task. Do not wait or keep working.")
		return sb.String()
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
					return tools.ToolApprovalDecision{Approved: true, ApprovalID: approval.ID}, nil
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

// notifyApprovalRequested pushes an approval notification to the person's
// reachable endpoints, best-effort. IM-originated approvals notify their own
// channel (unchanged). CLI-originated approvals used to be silently dropped;
// they now fan out to the person's OTHER bound accounts whose platform has a
// delivery sender, so a run started in a terminal can be approved from WeChat
// or Telegram (scenario 1 of docs/identity-continuity.md). Failures are
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
	// Origin is CLI when the PLATFORM is cli — the channel is a session UUID
	// for TUI turns and the literal "cli" only for `selfmind send`, so matching
	// on channel silently routed TUI-originated approvals to a nonexistent
	// "cli" sender (observed live: approval push stuck in 'sending' forever
	// while the run waited on a human who was never notified).
	if identity.Platform != "cli" {
		// IM-originated: notify the requesting channel only (the person is
		// already looking at it); no cross-channel duplication.
		msg := base
		msg.Platform = identity.Platform
		msg.PlatformUserID = identity.PlatformUserID
		msg.Channel = channel
		_ = c.srv.Delivery.EnqueueAndTry(ctx, msg)
		return
	}
	c.fanOutToBoundIM(ctx, identity, base)
}

// fanOutToBoundIM delivers a message to every bound account of the person that
// has a push-capable delivery sender, skipping cli-like bindings (no push
// surface) and the originating account. Shared by approval notifications and
// CLI-originated async results so anything that happens in a terminal is
// visible on the person's IM endpoints (docs/identity-continuity.md).
func (c *RunCoordinator) fanOutToBoundIM(ctx context.Context, identity *control.IdentityContext, base delivery.Message) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || c.srv.Control == nil || identity == nil {
		return
	}
	accounts, err := c.srv.Control.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return
	}
	for _, account := range accounts {
		// The CLI account is the originating surface (it gets the inline y/N
		// prompt); other cli-like bindings have no push channel either.
		if account.Platform == "cli" || account.ID == identity.AccountID {
			continue
		}
		if !c.srv.Delivery.SupportsPlatform(account.Platform) {
			continue
		}
		msg := base
		msg.Platform = account.Platform
		msg.PlatformUserID = account.PlatformUserID
		// Without a live chat context the platform user id is the DM target;
		// senders fall back to it when Channel is not a real chat id.
		msg.Channel = account.PlatformUserID
		_ = c.srv.Delivery.EnqueueAndTry(ctx, msg)
	}
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
	if !strings.HasPrefix(lower, "/") {
		return false, "", nil
	}
	switch {
	case lower == "/help":
		return true, strings.TrimSpace(`SelfMind commands:
/help                 Show this help.
/model                Show the configured model.
/id                   Show your resolved account identity.
/status               Show the current task status.
/tasks                List recent tasks.
/events               List recent events for the current task.
/approvals            List pending approvals.
/approve <n|id>       Approve a pending action (list number or apr_ id).
/reject <n|id>        Reject a pending action (list number or apr_ id).
/stop                 Cancel the active run.
/new [title]          Create a new task.
/resume <task_id>     Resume a task.
/workspace <id>       Select a workspace.
/workspaces           List workspaces.`), nil
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
			return true, "No active run to stop.", nil
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
		return true, fmt.Sprintf("Resumed task: %s (%s)", task.Title, task.ID), nil
	case lower == "/tasks" || lower == "tasks" || strings.Contains(lower, "任务列表"):
		tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		if err != nil {
			return true, "", err
		}
		return true, formatTasks(tasks), nil
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
		return false, "", nil
	}
}

// statusReply builds the /status card shared by every channel's control-command
// path. When the person has an active run, its task is what the user is
// waiting on, so report it first (an async run may attach to a task the
// current_task pointer has not caught up with yet); fall back to the
// per-person current_task pointer otherwise. The card format itself
// (formatTaskStatus, `Task:` / `Status:` markers) is a stable contract pinned
// by the continuity eval suite — change it there, not here.
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
	return card, nil
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
