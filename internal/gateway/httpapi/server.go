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
	// PendingNotifyAfter is the escrow threshold (Fix 2): an unanswered
	// approval/clarify older than this, whose person has detached from the CLI and
	// which was never notified, is re-pushed to the preferred IM by the periodic
	// sweep. Zero disables escrow.
	PendingNotifyAfter time.Duration
	// ApprovalJudge is the optional cheap-model judge for smart-mode LLM approval
	// triage (H2). Installed on every execution scope so a dangerous
	// (non-hardline) op in smart mode is triaged before the human ask. Nil (e.g.
	// eval, or no cheap model) makes smart mode degrade to a human ask — never an
	// auto-approval.
	ApprovalJudge tools.ApprovalJudge
	// Recall is the automatic semantic-recall engine (Work Timeline P2): at
	// turn start the context selector attaches bounded "possibly related prior
	// work" slices from the FTS session index and task label cards to the
	// runtime context. Nil disables automatic recall (the model-invoked
	// session_search tool is unaffected). See recall.go.
	Recall *RecallEngine
	// Sessions backs the structured /v1/sessions endpoint (search / recent /
	// message window) for thin clients — the daemon-side session index, always
	// queried on the PERSON partition. Nil = 503 (handlers_sessions.go).
	Sessions SessionsBackend
	// TaskGovernance controls reversible task hygiene. It never changes
	// context selection: tasks remain display/resume labels over the person
	// work spine. See task_governance.go.
	TaskGovernance TaskGovernanceOptions
	// PostRunAnalyzer performs at most one cheap-model maintenance pass after
	// an eligible run: task-label hygiene plus durable fact extraction. Nil
	// keeps the pre-label and skips automatic learning without affecting the
	// completed user turn.
	PostRunAnalyzer PostRunAnalyzer
	// PostRunMaintenance controls the daemon-owned debounce/batch window for
	// post-run label and memory governance. It never delays run finalization.
	PostRunMaintenance PostRunMaintenanceOptions
	// MemoryConsolidator is the background memory self-organization pass
	// (docs/memory-governance.zh-CN.md §4). Nil disables governance entirely;
	// the loop is started by the gateway runner via StartMemoryGovernance.
	MemoryConsolidator MemoryConsolidator
	// AttachmentsDir is the person-partitioned store for message attachments
	// (e.g. TUI clipboard-pasted images). importAttachments copies inbound
	// attachment files here (<AttachmentsDir>/<personID>/<runID>/NN-name) and
	// the person's partition joins that run's ExecutionScope AllowedRoots so
	// file tools can read them — external temp paths stay out of scope. Empty
	// disables importing (attachments keep their original client paths).
	AttachmentsDir string
	// ToolOutputDir is the spool root for over-budget tool outputs
	// (execution-quality W1): each run's sink writes
	// <ToolOutputDir>/<personID>/<artifactID>.txt and the tool_output_view
	// tool reads them back person-scoped. Empty disables spooling (truncation
	// notes stay plain head/tail).
	ToolOutputDir string
	// SkillReviewer executes durable background-review jobs (execution-quality
	// W7); nil disables the worker's review pass (reviews stay in-process).
	SkillReviewer SkillReviewRunner

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

	// eventBroker is the daemon's person-scoped real-time event plane. Durable
	// events still live in control.db; assistant deltas remain ephemeral.
	eventBroker     *runEventBroker
	eventBrokerOnce sync.Once
}

type activeRun struct {
	TenantID       string
	PersonID       string
	TaskID         string
	RunID          string
	QueueID        string
	Channel        string
	Platform       string
	PlatformUserID string
	WorkspaceID    string
	ApprovalMode   string
	Summary        string
	StartedAt      time.Time
	Cancel         context.CancelFunc
	// Interrupt supplies an infrastructure cause distinct from an explicit
	// user cancellation. Gateway restarts are resumable and must never turn a
	// task into a user-cancelled terminal state.
	Interrupt context.CancelCauseFunc
	// Steer carries mid-turn user guidance into the running agent loop
	// (installed on the run ctx via kernel.WithSteering; drained at iteration
	// boundaries). Buffered so /v1/runs/steer can hand off without blocking the
	// HTTP handler; a full buffer is reported as back-pressure, never dropped.
	Steer chan kernel.SteeringInput
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
	mux.HandleFunc("/v1/runs/stream", d.handleRunStream)
	mux.HandleFunc("/v1/events/stream", d.handleEventsStream)
	mux.HandleFunc("/v1/sessions", d.handleSessions)
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

// presenceClaimed reports whether the request claims interactive presence.
// The client stamps active=0 on heartbeats/polls once its last user INPUT is
// older than the configured presence idle timeout (input age is only known
// client-side); absent or any other value keeps the old always-attached
// behavior. Presence stays derived: the daemon just honors the claim.
func presenceClaimed(r *http.Request) bool {
	return r.URL.Query().Get("active") != "0"
}

// handlePresencePing is the lightweight attachment heartbeat for idle
// interactive clients (the TUI pings every 30s while open). It resolves
// identity exactly like the other endpoints and only touches derived presence
// state — it never mutates tasks, runs, or approvals. A beat with active=0
// (no recent user input at that terminal) is a no-op on presence, so a
// vacated TUI detaches by TTL and pushes route to the preferred IM again.
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
	if presenceClaimed(r) {
		d.touchPresence(r.Context(), identity)
	}
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
	if containsUnresolvedPasteToken(req.Content) {
		msg := "The pasted content was not expanded by the client. Paste it again and retry."
		return api.MessageResponse{Error: msg, Turn: messageTurn("failed", "", "", "", "", msg)}, http.StatusBadRequest
	}

	identity, err := d.Control.ResolveOrCreateAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return api.MessageResponse{Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
	}
	// Any inbound message is a presence beat for its endpoint: a CLI turn
	// marks the terminal attached, an IM message refreshes that account's
	// recency for preferred-endpoint selection.
	d.touchPresence(ctx, identity)
	// An IM inbound also just refreshed the platform session (the weixin
	// adapter saved the fresh context_token before dispatching here), which is
	// the one moment a sent_unconfirmed push can be re-delivered and actually
	// arrive. Fire-and-forget with its own bounded ctx; the delivery layer owns
	// the anti-duplicate rails (at-most-once claim, freshness window, cap).
	if d.Delivery != nil && req.Platform != "" && req.Platform != "cli" && req.Platform != "eval" {
		go func(tenantID, personID, platform, channel string) {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d.Delivery.CatchUpRecoverable(cctx, tenantID, personID, platform, channel)
		}(identity.TenantID, identity.PersonID, req.Platform, req.Channel)
	}

	if handled, content, err := d.tryHandleControlCommand(ctx, identity, req); handled {
		if err != nil {
			return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
		}
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK
	}
	// A command-SHAPED message that no control command claimed is a mistyped
	// COMMAND, not conversation or new work. Reject it here so it can never be
	// dispatched to the agent or queued behind a running task (observed live:
	// "/qwer" created a task and joined the queue). Skill slash-invocation on
	// the gateway uses /v1/dispatch, not this message path, so rejecting every
	// unrecognized command-shaped token here is safe. A "/"-leading path or
	// other prose ("/mnt/c/pic.png 看一下") is NOT command-shaped and stays on
	// the agent-first message path (command.LooksLikeCommand).
	if command.LooksLikeCommand(req.Content) {
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
		// A continuation targets the ACTIVE task, so it is not new work and must
		// never be queued. Historically this returned a bare "busy" reply, which
		// left the cross-endpoint takeover story broken: a continuation arriving
		// from IM/web (or any entry that isn't the thin-client /v1/runs/steer
		// path) could not reach the in-flight run. Now every entry steers — the
		// continuation is injected into the active run's steering channel, the
		// same channel /v1/runs/steer uses — so guidance from any surface lands
		// in the running task (docs/identity-continuity.md "Runtime attachment
		// model"). Genuinely new work still enqueues (G1+G2), never rejected as
		// "busy".
		if intent.Intent == router.IntentContinue {
			if resp, ok := d.steerActiveRun(ctx, identity, running, req); ok {
				return resp, http.StatusOK
			}
			// Steering unavailable (no live channel) or the buffer is full
			// (guidance arriving faster than the loop drains): report back-
			// pressure honestly rather than dropping the message.
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
	var deadlineCancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		runCtx, deadlineCancel = context.WithDeadline(runCtx, deadline)
	}
	runCtx, cancelCause := context.WithCancelCause(runCtx)
	cancel := func() { cancelCause(context.Canceled) }
	defer cancel()
	if deadlineCancel != nil {
		defer deadlineCancel()
	}
	// The steering channel is registered on the active run (so /v1/runs/steer
	// can reach it) AND installed on the run ctx (so the agent loop drains it).
	steerCh := make(chan kernel.SteeringInput, steerBufferSize)
	if ok := coord.beginActive(identity.PersonID, &activeRun{
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
		Cancel:         cancel,
		Interrupt:      cancelCause,
		Steer:          steerCh,
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
	runCtx = kernel.WithSteeringInputs(runCtx, steerCh)

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

// taskAttach describes how resolveTask chose the turn's task label, so later
// stages know whether the choice was the user's or a guess.
type taskAttach struct {
	// created is true when a brand-new placeholder label was created for this
	// turn. Its title is provisional (truncated input) until the post-run
	// labeler titles it once; the labeler may also fold it into an existing
	// open label and delete the empty placeholder.
	created bool
	// preLabel is true when the label is a soft pre-label GUESS (no explicit
	// continuation evidence): the post-run labeler may re-point the run, and
	// the execution workspace follows the REQUEST, not the guessed label.
	preLabel bool
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

func redactApprovalArgs(args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	for k, v := range args {
		out[k] = tools.RedactSensitive(fmt.Sprintf("%v", v))
	}
	return out
}

// approvalActionTarget derives a compact single-string object of the pending
// action (a path, command, pattern, or name) from the tool args, redacted like
// the stored args. One-line UI surfaces use it; the full redacted args stay on
// the approval row.
func approvalActionTarget(args map[string]interface{}) string {
	for _, key := range []string{"path", "file_path", "filename", "command", "pattern", "query", "name", "action", "url"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return tools.RedactSensitive(strings.TrimSpace(v))
		}
	}
	return ""
}

// personSettingNotifyPlatform is the person_settings key holding the explicit
// /notify endpoint preference; empty/absent means "auto" (most recently seen).
const personSettingNotifyPlatform = "notify_platform"

// personSettingApprovalMode is the person_settings key holding the person's
// persisted /mode (approval policy). Applied when a request carries no explicit
// per-request mode; empty/absent means on-request.
const personSettingApprovalMode = "approval_mode"

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

// appendApprovalModeEvent records a task-visible audit trail when a /mode change
// auto-settles a pending approval, so the timeline shows WHY a stuck approval
// suddenly resolved. Best-effort: a lost event never affects correctness.
func (d *Server) appendApprovalModeEvent(ctx context.Context, ap control.ApprovalRequest, eventType, mode string) {
	if ap.TaskID == "" {
		return
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     ap.TaskID,
		RunID:      ap.RunID,
		Type:       eventType,
		Visibility: "task",
		Payload: mustJSON(map[string]string{
			"approval_id": ap.ID,
			"reason":      "approval mode changed to " + mode,
		}),
	})
}

// pluralize renders "1 thing" / "n things" for a compact count phrase.
func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// suggestControlCommand returns the closest control command when the first
// token is a near-miss, delegating to the shared registry so the gateway and
// the TUI make the SAME unknown-command decision.
func suggestControlCommand(lower string) string {
	return command.Suggest(lower)
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
