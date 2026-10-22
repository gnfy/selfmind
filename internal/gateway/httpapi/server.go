package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

type Server struct {
	// BackgroundRunContext is an optional embedding-owned lifetime/telemetry root
	// for detached work. It must be installed before serving and never come from
	// a client request. Nil preserves the standalone daemon background lifetime.
	ExecutionRuntime     *tools.ExecutionRuntime
	BackgroundRunContext context.Context
	Control              *control.Store
	Gateway              *router.Gateway
	// MainRoutingProvider is the foreground Main transport used only for a
	// bounded, tool-free coordination Run when IM has several live work Runs.
	// Nil keeps the durable human choice fallback.
	MainRoutingProvider llm.Provider
	Delivery            *delivery.Service
	DefaultTenantID     string
	// PromptSnapshotHash pins durable background jobs to the static prompt
	// revision active when their evidence was materialized.
	PromptSnapshotHash string
	DrainTimeout       time.Duration
	ShutdownFunc       func()
	RuntimeStatusFunc  func() api.GatewayRuntimeInfo
	// ToolSchemaReportFunc exposes the registration-time, redacted schema
	// catalogue. It contains hashes and issue classes only, never raw external
	// schemas. Nil keeps test/minimal servers independent from a dispatcher.
	ToolSchemaReportFunc func() []tools.ToolSchemaReport
	// ToolCatalogPreviewFunc exposes the exact provider-wire foreground
	// catalogue after exposure and strategy filtering. The report contains only
	// names, counts, hashes, and issue classes; no schemas or credentials.
	ToolCatalogPreviewFunc func(context.Context) llm.ToolCatalogPreview
	// ToolCatalogProbeFunc performs the explicit, bounded live primary-provider
	// probe requested by a local CLI doctor. It is never called by ordinary
	// status requests.
	ToolCatalogProbeFunc func(context.Context) api.ProviderToolCatalogProbeResponse
	// ModelProbeFunc runs the same bounded role-aware provider contract probe as
	// the CLI, but inside the daemon environment. First-use setup calls it after
	// installing the OS service so shell-only credentials cannot produce a false
	// success. It is local-CLI-only and never runs on ordinary status requests.
	ModelProbeFunc func(context.Context, string) api.ModelProbeResponse
	// ModelChanges owns the daemon-serialized, non-secret route transaction
	// state. UI and channel adapters only render or submit its commands.
	ModelChanges *modelchange.Service

	// background owns the post-run maintenance and memory-governance workers so
	// every readiness transition has one place to (re)start them.
	background backgroundServices
	// ModelRestartFunc starts the normal external restart helper after a model
	// transaction is committed. The helper, rather than the daemon itself,
	// preserves launchd/systemd ownership and on-demand restart behavior.
	ModelRestartFunc func(string) error
	// RestartCapability is a secret only this daemon and the restart helper it
	// starts know. It lets that helper, which is a descendant of the daemon and so
	// never the verified local CLI, stop the daemon for the model change that
	// started it, and for nothing else. Empty accepts no capability.
	RestartCapability string
	// ListenAddr is the TCP address the daemon accepts requests on. A loopback
	// address makes every TCP request name a loopback Host, so a page that
	// rebinds its own name to 127.0.0.1 reaches nothing. Empty, or an address
	// beyond loopback, leaves the Host unchecked: the names a remote client
	// reaches the daemon by are not known.
	ListenAddr string
	// refusals limits the warnings written for refused peers, so a process that
	// keeps asking cannot fill the daemon's log.
	refusals refusalLog
	// MCPHealthFunc exposes connection/catalog failures without credentials or
	// raw schemas so status and doctor do not depend on daemon log inspection.
	MCPHealthFunc func() tools.MCPHealthSnapshot
	// MCPServersFunc lists the configured MCP servers, with the identity a
	// person's read-only marking is bound to, for /tools readonly. Nil means
	// no server is configured.
	MCPServersFunc func() []tools.MCPServerStatus
	// SkillStorage is the daemon-resolved immutable asset base. Direct selector
	// and management paths use the same root as dispatcher middleware instead
	// of consulting the daemon process HOME independently.
	SkillStorage *tools.SkillStorage
	// Schedules is the cron store, read when the local command line adopts a
	// schedule no one can be shown to own. Nil without a scheduler.
	Schedules *cron.Scheduler
	// CronJobs answers whether the schedule a queued scheduled fire came from
	// still exists and is enabled. Nil reads Schedules; with neither the check
	// is skipped, as without a scheduler there is nothing to ask.
	CronJobs CronJobLookup
	// PendingNotifyAfter is the escrow threshold (Fix 2): an unanswered
	// approval/clarify older than this, whose person has detached from the CLI and
	// which was never notified, is re-pushed to the preferred IM by the periodic
	// sweep. Zero disables escrow.
	PendingNotifyAfter time.Duration
	// ApprovalWait bounds how long a run parks on an unanswered approval while
	// an endpoint could still answer, and ApprovalWaitUnattended is the much
	// shorter bound used when nothing can currently answer (no attached endpoint,
	// no routable IM account, or an unhealthy latest IM delivery state). Both are
	// resolved from config by `app`; zero means the
	// package default. Neither ever changes the OUTCOME of a timeout — an
	// unanswered approval parks the work, it is never a rejection.
	ApprovalWait           time.Duration
	ApprovalWaitUnattended time.Duration
	// HumanWaitGrace is a short in-place grace before a checkpointed human wait
	// releases its worker. Zero uses the default; injected by focused tests.
	HumanWaitGrace time.Duration
	// HumanWaitHold is how long an unanswered human wait may keep its Run in
	// place while its person is at the terminal that asked and nothing else needs
	// the slot. Zero uses the default, negative turns the hold off (every wait
	// then releases its worker after the grace); injected by focused tests.
	HumanWaitHold      time.Duration
	ExecutionWaitGrace time.Duration
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
	// an eligible run: explicit preference decisions plus Task Reference search
	// hints. It never routes tasks. Nil skips automatic learning without
	// affecting the completed user turn.
	PostRunAnalyzer PostRunAnalyzer
	// PostRunMaintenance controls the daemon-owned debounce/batch window for
	// post-run preference/reference governance. It never delays run finalization.
	PostRunMaintenance PostRunMaintenanceOptions
	// SelfEvolution profiles completed runs and records bounded read-only
	// batching observations. Runtime advice requires separately verified
	// comparison evidence; ordinary profiles never promote a candidate.
	SelfEvolution control.EvolutionPolicy
	// DisableAutomaticRunRecovery is the fail-closed operational rollback for
	// daemon/provider interruption continuation. Durable plans, tool evidence,
	// and explicit /resume remain available; only daemon-owned child scheduling
	// stops. False is the production default and preserves minimal tests.
	DisableAutomaticRunRecovery bool
	// MemoryConsolidator is the background memory self-organization pass
	// (docs/memory-governance.zh-CN.md §4). Nil disables governance entirely;
	// the loop is started by the gateway runner via StartMemoryGovernance.
	MemoryConsolidator MemoryConsolidator
	// Memory backs the cross-endpoint explicit memory commands
	// (/remember, /forget). Nil leaves the commands answering that memory is
	// unavailable; nothing else consults it here.
	Memory *memory.MemoryManager
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
	// SkillCurator is the sole model-backed Skill proposal authority. It only
	// writes candidate versions from bounded comparable cohorts; active files
	// remain unchanged until deterministic promotion policy approves them.
	SkillCurator SkillCuratorRunner

	mu              sync.Mutex
	draining        bool
	drainReason     string
	shutdownPending bool

	runs     *RunCoordinator
	runsOnce sync.Once
	// taskLists remembers the exact numbered open-task cards most recently
	// rendered on each endpoint. Ordinal commands resolve this snapshot instead
	// of re-sorting mutable task rows after the person has already seen them.
	taskLists taskListSnapshotRegistry

	// presence is the in-memory endpoint-attachment registry (presence.go).
	// Lazily built like the coordinator so every Server construction path
	// (HTTP handlers, IM adapters, eval harness, tests) gets one.
	presence     *presenceRegistry
	presenceOnce sync.Once

	// eventBroker is the daemon's person-scoped real-time event plane. Durable
	// events still live in control.db; assistant deltas remain ephemeral.
	eventBroker     *runEventBroker
	eventBrokerOnce sync.Once

	// inboundSettleTimeout bounds each durable inbound receipt transition;
	// zero uses the default.
	inboundSettleTimeout time.Duration
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
	Origin         string
	ExecutionRoots []executionenv.RootBinding
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
	mux.HandleFunc("/v1/workspaces/trust", d.handleWorkspaceTrust)
	mux.HandleFunc("/v1/schedules/adopt", d.handleScheduleAdopt)
	mux.HandleFunc("/v1/workspaces/capabilities", d.handleWorkspaceCapabilities)
	mux.HandleFunc("/v1/workspaces/observation-profiles", d.handleWorkspaceObservationProfiles)
	mux.HandleFunc("/v1/workspaces/effect-profiles", d.handleWorkspaceEffectProfiles)
	mux.HandleFunc("/v1/workspaces", d.handleWorkspaces)
	mux.HandleFunc("/v1/gateway/status", d.handleGatewayStatus)
	mux.HandleFunc("/v1/gateway/tool-catalog/probe", d.handleGatewayToolCatalogProbe)
	mux.HandleFunc("/v1/gateway/model/probe", d.handleGatewayModelProbe)
	mux.HandleFunc("/v1/gateway/model/change", d.handleGatewayModelChange)
	mux.HandleFunc("/v1/gateway/shutdown", d.handleGatewayShutdown)
	mux.HandleFunc("/v1/presence/ping", d.handlePresencePing)
	mux.HandleFunc("/v1/digest", d.handleDigest)
	mux.HandleFunc("/v1/runs/view", d.handleRunView)
	return d.requireLoopbackHost(rejectBrowserRequests(mux))
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

// presenceClaimed reports endpoint liveness. The legacy active=0 query value is
// intentionally ignored: keyboard silence is normal during long agent turns
// and must not masquerade as a closed terminal. Detachment is heartbeat expiry.
func presenceClaimed(r *http.Request) bool {
	return r != nil
}

// handlePresencePing is the lightweight attachment heartbeat for idle
// interactive clients (the TUI pings every 30s while open). It resolves
// identity exactly like the other endpoints and only touches derived presence
// state — it never mutates tasks, runs, or approvals. Old active=0 clients are
// treated as live; an unanswered request escalates by request age instead.
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
		writeIdentityError(w, err)
		return
	}
	if presenceClaimed(r) {
		d.touchPresence(r.Context(), identity)
	}
	if n, err := strconv.ParseInt(r.URL.Query().Get("foreign_session_events"), 10, 64); err == nil && n > 0 {
		total := d.events().recordClientDrops(identity.PersonID, n)
		log.Warn("terminal dropped other-session detail events it should never have received",
			"person", identity.PersonID, "platform", identity.Platform, "count", n, "total_since_start", total)
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
	ctx := r.Context()
	if cliClaim(req.Platform) {
		if !requestLocalPeer(r).Verified {
			writeError(w, http.StatusForbidden, d.newLocalControlRefusal(r, "CLI identity"))
			return
		}
		ctx = withLocalFilesystemAuthority(ctx)
	}
	resp, status := d.ProcessMessage(ctx, req)
	writeJSON(w, status, resp)
}

func (d *Server) ProcessMessage(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
	resp, status, _ := d.processMessage(ctx, req)
	return resp, status
}

// notDispatched explains a refusal that happened before anything acted on the
// input: no command ran, and no choice, steer, queue row, or Run exists for
// it. Only such a refusal lets a durable inbound receipt return to pending; any
// later failure, including a 5xx, may follow a real effect. The reason is a
// short class a chat may show; the cause stays in logs.
type notDispatched struct {
	reason string
	cause  error
	// consumedChoice marks a refusal that came after the input claimed a
	// continuity choice. The claim cleared the choice's saved request, so a
	// redelivered answer could not replay it and the receipt is held instead.
	consumedChoice bool
}

// afterChoiceClaim marks a refusal of input that already claimed a continuity
// choice; see notDispatched.consumedChoice.
func afterChoiceClaim(req api.MessageRequest, refusal *notDispatched) *notDispatched {
	if refusal == nil || strings.TrimSpace(req.ContinuityAction) == "" {
		return refusal
	}
	held := *refusal
	held.consumedChoice = true
	return &held
}

// exactWrite is the context for a store write whose error must mean nothing
// was written, such as one a refusal reports as failed. The SQLite driver
// reports an ended context even after its statement committed, so such a
// write sees neither the caller's cancellation nor a deadline; the store's
// busy timeout still bounds lock waits.
func exactWrite(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// Refusal reasons shown to senders. Internal errors never reach a chat.
const (
	refusalShutdown = "SelfMind is shutting down"
	refusalIdentity = "SelfMind could not identify the sender"
	refusalRecords  = "SelfMind could not read its records"
	refusalSave     = "SelfMind could not save the message"
)

// shuttingDown reports a process drain that refuses new input. A model-change
// drain is different: it still queues input for the restarted daemon.
func (d *Server) shuttingDown() bool {
	return d.IsDraining() && !d.isModelChangeDrain()
}

func shutdownRefusal(identity *control.IdentityContext) (api.MessageResponse, int, *notDispatched) {
	return api.MessageResponse{
		Identity: identity,
		Error:    "gateway is shutting down; try again after restart",
		Accepted: false,
		Turn:     messageTurn("failed", "", "draining", "", "", "gateway is shutting down"),
	}, http.StatusServiceUnavailable, &notDispatched{reason: refusalShutdown}
}

// readRefusal reports a store read that failed before anything was claimed.
func readRefusal(identity *control.IdentityContext, err error) (api.MessageResponse, *notDispatched) {
	return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())},
		&notDispatched{reason: refusalRecords, cause: err}
}

// processMessage is ProcessMessage plus the typed refusal a durable inbound
// boundary needs to tell "nothing ran" from an uncertain failure.
func (d *Server) processMessage(ctx context.Context, req api.MessageRequest) (resp api.MessageResponse, status int, refusal *notDispatched) {
	defer func() {
		if err := d.enqueueTurnChoiceNotice(ctx, req, &resp); err != nil {
			// The choice already holds the input. Never call this not-dispatched
			// or accept an inbound receipt without its actionable notification.
			resp.Error = fmt.Sprintf("Your request is saved as choice %s, but its notice could not be saved.\n%s", resp.Choice.ID, resp.Content)
			log.Warn("choice notice could not be saved; inbound held", "choice", resp.Choice.ID, "error", err)
			status = http.StatusServiceUnavailable
		}
	}()
	req.TenantID = d.tenantID(req.TenantID)
	req.Platform = fallback(req.Platform, "cli")
	req.PlatformUserID = fallback(req.PlatformUserID, "local")
	req.Channel = fallback(req.Channel, req.Platform)
	req.Content = strings.TrimSpace(req.Content)
	if len(req.ClientAdditionalRoots) > 0 && !hasLocalFilesystemAuthority(ctx) {
		msg := "--add-dir is available only to an authenticated local CLI connected to a loopback gateway"
		return api.MessageResponse{Error: msg, Turn: messageTurn("failed", "", "", "", "", msg)}, http.StatusForbidden, nil
	}
	if req.Content == "" {
		return api.MessageResponse{Error: "content is required", Turn: messageTurn("failed", "", "", "", "", "content is required")}, http.StatusBadRequest, nil
	}
	if containsUnresolvedPasteToken(req.Content) {
		msg := "The pasted content was not expanded by the client. Paste it again and retry."
		return api.MessageResponse{Error: msg, Turn: messageTurn("failed", "", "", "", "", msg)}, http.StatusBadRequest, nil
	}
	// Only the person's own input is authority. Everything below up to
	// admission — first contact, presence, IM catch-up, continuity controls,
	// choice and question answers, commands — belongs to a person at an
	// endpoint; a turn the daemon started takes none of it.
	if !isUserOriginTurn(ctx, req) {
		return d.processDaemonTurn(ctx, req)
	}

	identity, err := d.Control.ResolveOrCreateAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return api.MessageResponse{Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError,
			&notDispatched{reason: refusalIdentity, cause: err}
	}
	if req.NativeReplyMessageID != "" && !command.LooksLikeCommand(req.Content) {
		edge, lookupErr := d.Control.NativeIMReplyTarget(ctx, identity.TenantID, identity.PersonID,
			req.Platform, req.Channel, req.NativeReplyMessageID)
		if lookupErr != nil || edge == nil || (edge.RunID == "" && edge.ApprovalID == "" && edge.ClarifyID == "") {
			content := "That replied-to message is not linked to your work here. Use /status and an exact run or request ID."
			if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
				resp, refusal := readRefusal(identity, lookupErr)
				return resp, http.StatusInternalServerError, refusal
			}
			return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}, http.StatusOK, nil
		}
		req.ReplyToRunID, req.ApprovalID, req.ClarifyID = edge.RunID, edge.ApprovalID, edge.ClarifyID
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
		go func(tenantID, personID, platform, platformUserID, channel string) {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d.Delivery.CatchUpRecoverable(cctx, tenantID, personID, platform, platformUserID, channel)
		}(identity.TenantID, identity.PersonID, req.Platform, identity.PlatformUserID, req.Channel)
	}
	// Explicit continuity controls are typed before the general command path:
	// /new <request> becomes a forced root turn, while /choose (or platform
	// choice metadata) restores the original pre-run request and exact target.
	// Identity is resolved again on this endpoint; stored prose is never parsed.
	if rewritten, handled, response, refusal := d.rewriteExplicitContinuityControl(ctx, identity, req); refusal != nil {
		return *response, http.StatusServiceUnavailable, refusal
	} else if handled {
		return *response, statusForMessageResponse(*response), nil
	} else {
		req = rewritten
	}
	if response, refusal := d.claimedTurnChoiceResponse(ctx, identity, req); refusal != nil {
		return *response, http.StatusServiceUnavailable, afterChoiceClaim(req, refusal)
	} else if response != nil {
		return *response, statusForMessageResponse(*response), nil
	}

	// A request restored from a claimed choice already carries the target the
	// person chose. Its saved text is not read again as a command, an answer,
	// or another choice: an approval or question that arrived after it was
	// written is not what it answers.
	if strings.TrimSpace(req.ContinuityAction) == "" {
		if handled, content, workspace, err := d.tryHandleControlCommand(ctx, identity, req); handled {
			if err != nil {
				return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError, nil
			}
			return api.MessageResponse{Identity: identity, Content: content, Workspace: workspace, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK, nil
		}
		// A bare number is a continuity answer only to the one recent choice
		// shown on this account and channel, and never when a reply, approval,
		// or clarification edge already names the target. Approval and
		// run-clarification answers ran first above and retain their priority.
		if rewritten, handled, response, refusal := d.rewriteBareTurnChoice(ctx, identity, req); refusal != nil {
			return *response, http.StatusServiceUnavailable, refusal
		} else if handled {
			return *response, statusForMessageResponse(*response), nil
		} else {
			req = rewritten
		}
		if response, refusal := d.claimedTurnChoiceResponse(ctx, identity, req); refusal != nil {
			return *response, http.StatusServiceUnavailable, afterChoiceClaim(req, refusal)
		} else if response != nil {
			return *response, statusForMessageResponse(*response), nil
		}
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
		return api.MessageResponse{Identity: identity, Content: msg, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK, nil
	}
	// Freeze an explicit selection into this input before admission/queueing.
	// An older queued request and another endpoint must never consume it later.
	if isUserOriginTurn(ctx, req) {
		selection, err := d.Control.TakeSessionResumeSelection(ctx, identity.TenantID, identity.PersonID, resumeSelectionEndpoint(identity, req.Channel))
		if err != nil {
			response, refusal := readRefusal(identity, err)
			return response, http.StatusServiceUnavailable, refusal
		}
		if selection != nil && !req.ForceNew && req.TaskID == "" && req.ReplyToRunID == "" && req.ApprovalID == "" && req.ClarifyID == "" && req.ContinuityAction == "" {
			req.ReplyToRunID = selection.RunID
		}
	}
	if response, handled, refusal := d.ambiguousActiveInput(ctx, identity, req); refusal != nil {
		return response, http.StatusServiceUnavailable, refusal
	} else if handled {
		return response, statusForMessageResponse(response), nil
	}
	return d.admitTurn(ctx, identity, req)
}

// admitTurn is where every source's turn meets the drain and admission: a
// model-change drain still queues work for the restarted daemon, a shutdown
// refuses it, and otherwise the turn is admitted.
func (d *Server) admitTurn(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageResponse, int, *notDispatched) {
	// The mode a new admission freezes is read before anything is accepted: a
	// preference that cannot be read refuses the turn here, retryably, instead
	// of after it was told it was accepted or under a guessed mode.
	if !d.IsDraining() || d.isModelChangeDrain() {
		frozen, err := d.freezeAdmissionMode(ctx, identity, req)
		if err != nil {
			resp, status, refusal := modeRefusal(identity, err)
			return resp, status, afterChoiceClaim(req, refusal)
		}
		req = frozen
	}
	if d.IsDraining() {
		if d.isModelChangeDrain() {
			intent := d.classifyIntent(ctx, req.Content, req.Channel)
			if req.ForceNew {
				intent = forcedNewIntent()
			}
			coord := d.coordinator()
			workspace, workspaceErr := coord.prepareRequestWorkspace(ctx, identity, &req)
			if workspaceErr != nil {
				return api.MessageResponse{Identity: identity, Error: workspaceErr.Error(), Turn: messageTurn("failed", "", "draining", "", "", workspaceErr.Error())}, http.StatusBadRequest, afterChoiceClaim(req, &notDispatched{reason: refusalRecords, cause: workspaceErr})
			}
			if rootsErr := coord.prepareRequestExecutionRoots(ctx, workspace, &req); rootsErr != nil {
				return api.MessageResponse{Identity: identity, Error: rootsErr.Error(), Turn: messageTurn("failed", "", "draining", "", "", rootsErr.Error())}, http.StatusBadRequest, afterChoiceClaim(req, &notDispatched{reason: refusalRecords, cause: rootsErr})
			}
			if running := coord.activeForIncoming(identity.PersonID, req); running != nil && (d.shouldSteerActiveNaturalInput(ctx, identity, req, running) || requestTargetsActiveRun(req, intent, running)) {
				if resp, ok := d.steerActiveRun(ctx, identity, running, req); ok {
					return resp, http.StatusOK, nil
				}
			}
			resp, refusal := d.enqueueDuringModelChange(ctx, identity, req)
			return resp, http.StatusOK, afterChoiceClaim(req, refusal)
		}
		// Choice answers are refused before their claim while shutting down;
		// only a claim that raced the start of the drain reaches here claimed.
		resp, status, refusal := shutdownRefusal(identity)
		return resp, status, afterChoiceClaim(req, refusal)
	}
	resp, status, refusal := d.dispatchAdmitted(ctx, identity, req)
	return resp, status, afterChoiceClaim(req, refusal)
}

// dispatchAdmitted handles input that passed every refusal that precedes
// admission. From here on a failure may follow a real effect; the one refusal
// left is a failed queue write, which leaves nothing behind for the input.
func (d *Server) dispatchAdmitted(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageResponse, int, *notDispatched) {
	// Control commands above remain reachable so /model can repair the sole
	// route authority. A continuation must still steer the already-frozen
	// active run so that a safe model restart can reach its boundary; genuinely
	// new work is parked without consulting an unverified model.
	if !d.modelReadyForWork() {
		coord := d.coordinator()
		// Resolve the execution scope BEFORE deciding to queue. Queuing first
		// stored a row with no workspace and no roots, so work submitted from one
		// directory could later drain against the person's default workspace —
		// an agent writing files in the wrong repository. The scope belongs to
		// the moment of submission, not to whenever the queue happens to drain,
		// and the model-change drain path beside this one already resolves first.
		workspace, workspaceErr := coord.prepareRequestWorkspace(ctx, identity, &req)
		if workspaceErr != nil {
			return api.MessageResponse{Identity: identity, Error: workspaceErr.Error(), Turn: messageTurn("failed", "", "idle", "", "", workspaceErr.Error())}, http.StatusBadRequest, &notDispatched{reason: refusalRecords, cause: workspaceErr}
		}
		if rootsErr := coord.prepareRequestExecutionRoots(ctx, workspace, &req); rootsErr != nil {
			return api.MessageResponse{Identity: identity, Error: rootsErr.Error(), Turn: messageTurn("failed", "", "idle", "", "", rootsErr.Error())}, http.StatusBadRequest, &notDispatched{reason: refusalRecords, cause: rootsErr}
		}
		running := coord.activeForIncoming(identity.PersonID, req)
		if running == nil {
			return admitted(d.enqueueUntilModelReady(ctx, identity, req))
		}
		intent := d.classifyIntent(ctx, req.Content, req.Channel)
		if req.ForceNew {
			intent = forcedNewIntent()
		}
		if (d.shouldSteerActiveNaturalInput(ctx, identity, req, running) || requestTargetsActiveRun(req, intent, running)) && isUserOriginTurn(ctx, req) {
			if resp, ok := d.steerActiveRun(ctx, identity, running, req); ok {
				return resp, http.StatusOK, nil
			}
			return api.MessageResponse{
				Identity: identity, Content: formatBusyRun(running), Accepted: false,
				Turn: messageTurn("busy", "running", "running", running.TaskID, running.RunID, running.Summary),
			}, http.StatusOK, nil
		}
		return admitted(d.enqueueUntilModelReady(ctx, identity, req))
	}

	// Active user work is Main-owned. Persist ordinary natural-language input
	// into the live Run before any general intent classification: the same Main
	// that already has the execution context is
	// the right place to decide whether the input refines current work or should
	// become independent queued work. Explicit controls/edges and daemon-origin
	// turns remain on their deterministic paths.
	if running := d.coordinator().activeForIncoming(identity.PersonID, req); running != nil &&
		d.shouldSteerActiveNaturalInput(ctx, identity, req, running) {
		coord := d.coordinator()
		workspace, workspaceErr := coord.prepareRequestWorkspace(ctx, identity, &req)
		if workspaceErr != nil {
			return api.MessageResponse{Identity: identity, Error: workspaceErr.Error(), Turn: messageTurn("failed", "", "running", running.TaskID, running.RunID, workspaceErr.Error())}, http.StatusBadRequest, &notDispatched{reason: refusalRecords, cause: workspaceErr}
		}
		if rootsErr := coord.prepareRequestExecutionRoots(ctx, workspace, &req); rootsErr != nil {
			return api.MessageResponse{Identity: identity, Error: rootsErr.Error(), Turn: messageTurn("failed", "", "running", running.TaskID, running.RunID, rootsErr.Error())}, http.StatusBadRequest, &notDispatched{reason: refusalRecords, cause: rootsErr}
		}
		if req.AdditionalRootsRequested && !sameCLIAdditionalRoots(req.ExecutionRoots, running.ExecutionRoots) {
			message := "Cannot change --add-dir roots while a run is active. Start a new run after the current run finishes."
			return api.MessageResponse{
				Identity: identity,
				Content:  message,
				Accepted: false,
				Turn:     messageTurn("failed", "running", "running", running.TaskID, running.RunID, message),
			}, http.StatusConflict, nil
		}
		if resp, ok := d.steerActiveRun(ctx, identity, running, req); ok {
			return resp, http.StatusOK, nil
		}
		// Persistence or live-channel back-pressure prevented immediate delivery.
		// The durable foreground queue is the lossless fallback; never acknowledge
		// input that exists only in memory.
		return admitted(d.enqueueBehindActive(ctx, identity, req))
	}

	intent := d.classifyIntent(ctx, req.Content, req.Channel)
	if req.ForceNew {
		intent = forcedNewIntent()
	}
	if handled, resp := d.tryHandleIntentClarification(identity, intent); handled {
		return resp, http.StatusOK, nil
	}
	if handled, resp := d.tryHandleDirectIntent(ctx, identity, req, intent); handled {
		return resp, http.StatusOK, nil
	}

	coord := d.coordinator()
	workspace, workspaceErr := coord.prepareRequestWorkspace(ctx, identity, &req)
	if workspaceErr != nil {
		return api.MessageResponse{Identity: identity, Error: workspaceErr.Error(), Turn: messageTurn("failed", "", "idle", "", "", workspaceErr.Error())}, http.StatusBadRequest, &notDispatched{reason: refusalRecords, cause: workspaceErr}
	}
	if rootsErr := coord.prepareRequestExecutionRoots(ctx, workspace, &req); rootsErr != nil {
		return api.MessageResponse{Identity: identity, Error: rootsErr.Error(), Turn: messageTurn("failed", "", "idle", "", "", rootsErr.Error())}, http.StatusBadRequest, &notDispatched{reason: refusalRecords, cause: rootsErr}
	}
	// Declared new work and daemon-originated work can use an independent
	// slot. A CLI foreground lane still serializes its own new turns.
	if running := coord.activeForIncoming(identity.PersonID, req); running != nil &&
		(req.Platform == "cli" || (!req.ForceNew && req.ContinuityAction != "new" && isUserOriginTurn(ctx, req))) {
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
		if requestTargetsActiveRun(req, intent, running) && isUserOriginTurn(ctx, req) {
			if req.AdditionalRootsRequested && !sameCLIAdditionalRoots(req.ExecutionRoots, running.ExecutionRoots) {
				message := "Cannot change --add-dir roots while a run is active. Start a new run after the current run finishes."
				return api.MessageResponse{
					Identity: identity,
					Content:  message,
					Accepted: false,
					Turn:     messageTurn("failed", "running", "running", running.TaskID, running.RunID, message),
				}, http.StatusConflict, nil
			}
			if resp, ok := d.steerActiveRun(ctx, identity, running, req); ok {
				return resp, http.StatusOK, nil
			}
			// Steering unavailable (no live channel) or the buffer is full
			// (guidance arriving faster than the loop drains): report back-
			// pressure honestly rather than dropping the message.
			return api.MessageResponse{
				Identity: identity,
				Content:  formatBusyRun(running),
				Accepted: false,
				Turn:     messageTurn("busy", "running", "running", running.TaskID, running.RunID, running.Summary),
			}, http.StatusOK, nil
		}
		return admitted(d.enqueueBehindActive(ctx, identity, req))
	}
	if !coord.hasCapacity(identity.PersonID) {
		return admitted(d.enqueueBehindActive(ctx, identity, req))
	}

	if req.Async {
		// The async worker owns a fresh context. Admit caller paths while their
		// authenticated local authority is still available; the worker only
		// receives managed references and must not inherit filesystem authority.
		req.Attachments = coord.importAttachments(ctx, identity, nil, req.Attachments)
		return admitted(coord.startAsyncRun(identity, req, intent))
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
	active := &activeRun{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Channel:        req.Channel,
		Platform:       req.Platform,
		PlatformUserID: req.PlatformUserID,
		WorkspaceID:    req.WorkspaceID,
		ApprovalMode:   req.ApprovalMode,
		Origin:         runOrigin(runCtx, req),
		ExecutionRoots: executionenv.CloneRootBindings(req.ExecutionRoots),
		QueueID:        req.QueueID,
		Summary:        truncate(req.Content, 240),
		StartedAt:      time.Now(),
		Cancel:         cancel,
		Interrupt:      cancelCause,
		Steer:          steerCh,
	}
	if ok, response, refusal := coord.admitActive(ctx, identity, req, active); !ok {
		return admitted(response, refusal)
	}
	// Durably transfer guidance that lost the final-model-call race while the
	// per-person slot is still held, then free the slot and drain. This ordering
	// prevents a new submission from overtaking acknowledged input during the
	// finalization boundary. The local pointer is the same object the coordinator
	// updates with TaskID/RunID after StartRun.
	defer func() {
		coord.deferUnconsumedSteering(identity, active)
		coord.endActiveRun(identity.PersonID, active)
		coord.drainQueue(identity)
	}()
	runCtx = kernel.WithSteeringInputs(withActiveRun(runCtx, active), steerCh)

	if isUserOriginTurn(ctx, req) && strings.EqualFold(strings.TrimSpace(identity.Platform), "cli") {
		stopPresence := make(chan struct{})
		defer close(stopPresence)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopPresence:
					return
				case <-ctx.Done():
					return
				case <-runCtx.Done():
					return
				case <-ticker.C:
					if ctx.Err() == nil && runCtx.Err() == nil {
						d.touchPresence(ctx, identity)
					}
				}
			}
		}()
	}

	resp, status := coord.runMessage(runCtx, identity, req, intent)
	// Detached finish: if the endpoint that dispatched this sync turn vanished
	// mid-run (the ORIGINAL request ctx was cancelled — not deadline-expired,
	// and the run itself was not cancelled), nobody reads the HTTP response,
	// so route the result like an async one: IM fan-out for cli-originated
	// runs, the source channel otherwise. A client that stayed connected gets
	// the sync answer only, unless the run explicitly pinned a cross-endpoint
	// delivery override (e.g., set_delivery_target to a bound IM channel).
	if !turnCancelled(resp) {
		detached := ctx.Err() != nil && context.Cause(ctx) == context.Canceled
		if detached || coord.hasCrossEndpointDeliveryOverride(context.Background(), identity, req, resp) {
			coord.deliverAsyncResult(context.Background(), identity, req, resp)
		}
	}
	return resp, status, nil
}

// shouldSteerActiveNaturalInput separates deterministic ownership from
// semantic interpretation. Ordinary user prose belongs to the live Main; an
// explicit new/other-task/other-run request, a one-shot /resume pin, or a
// daemon-originated turn must retain its existing control-plane path.
func (d *Server) shouldSteerActiveNaturalInput(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, running *activeRun) bool {
	if d == nil || d.Control == nil || identity == nil || running == nil || !isUserOriginTurn(ctx, req) || req.ForceNew {
		return false
	}
	if taskID := strings.TrimSpace(req.TaskID); taskID != "" && taskID != running.TaskID {
		return false
	}
	if replyRunID := strings.TrimSpace(req.ReplyToRunID); replyRunID != "" {
		return replyRunID == running.RunID
	}
	// Each CLI window has its own channel and default work focus. A fresh
	// instruction in window B must not become guidance to window A merely
	// because A is the person's current executor. With the present one-run
	// admission limit it queues for B; exact reply edges can still target A.
	if req.Platform == "cli" && strings.TrimSpace(req.Channel) != running.Channel {
		return false
	}
	switch strings.TrimSpace(req.ContinuityAction) {
	case string(ContinuityResume), "new":
		return false
	}
	return true
}

// requestTargetsActiveRun is the sole busy-path steering predicate. An exact
// reply edge wins. A typed historical RESUME must stay queued for its own
// parent instead of being redirected into whichever run happens to be active.
func requestTargetsActiveRun(req api.MessageRequest, intent router.IntentResult, running *activeRun) bool {
	if running == nil {
		return false
	}
	if replyRunID := strings.TrimSpace(req.ReplyToRunID); replyRunID != "" {
		return replyRunID == running.RunID
	}
	switch strings.TrimSpace(req.ContinuityAction) {
	case string(ContinuityResume), "new":
		return false
	case string(ContinuitySteer):
		return false // a typed steer always carries an exact run edge
	default:
		return false
	}
}

func statusForMessageResponse(resp api.MessageResponse) int {
	if strings.TrimSpace(resp.Error) != "" {
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

func forcedNewIntent() router.IntentResult {
	return router.IntentResult{
		Intent: router.IntentTask, Confidence: 1, Reason: "explicit /new request",
		Signals: []string{"control.new"}, ShouldCreateTask: true, ShouldUseTools: true,
		Source: "control",
	}
}

func (d *Server) isModelChangeDrain() bool {
	_, draining, reason := d.gatewayStateParts()
	return draining && strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), "model_change:")
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

func (d *Server) messageContextBudget(usage llm.UsageStats) *api.ContextBudgetInfo {
	budget := kernel.DefaultRuntimeContextBudget()
	if d != nil && d.Gateway != nil {
		budget = d.Gateway.RuntimeContextBudget()
	}
	return contextBudgetInfo(usage, budget)
}

func contextBudgetInfo(usage llm.UsageStats, budget kernel.RuntimeContextBudget) *api.ContextBudgetInfo {
	return &api.ContextBudgetInfo{
		TotalChars:            budget.TotalChars,
		WorkspaceChars:        budget.WorkspaceChars,
		TaskChars:             budget.TaskChars,
		MemoryChars:           budget.MemoryChars,
		SkillMainBytes:        budget.SkillMainBytes,
		SkillMainTokens:       budget.SkillMainTokens,
		SkillCatalogBytes:     budget.SkillCatalogBytes,
		SkillCatalogTokens:    budget.SkillCatalogTokens,
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
	// reason records the deterministic evidence that selected this label. A
	// display-only extracted work key is metadata, not a selection reason: one
	// issue may contain several independent work lines, so sharing a token is
	// not authority to attach or close prior work.
	reason taskAttachReason
	// workKey records a deterministic issue key used at ingress. It does not
	// make the label a context/workspace boundary; it only makes the display
	// decision auditable after the run id exists.
	workKey string
	// effectKey is present only for durable system work whose logical products
	// must remain exactly-once across retry runs.
	effectKey string
	// policy is the typed authority boundary for this attach. The legacy
	// created/preLabel fields remain replay metadata for post-run label hygiene;
	// they must not be consulted for workspace, context, pointer, or run
	// ownership decisions.
	policy              attachPolicy
	matchedSurfaceForms []string
	candidateTaskIDs    []string
	candidateTaskHints  []string
	// resumesRunID is structured parent evidence resolved at attach time (a
	// platform reply or an approval's origin run). It names the ONLY run this
	// turn may continue; ambiguity resolution is skipped for it, and a named
	// run that is terminal or already claimed simply yields no parent.
	resumesRunID string
}

type attachContextMode string

const (
	attachContextNone    attachContextMode = "none"
	attachContextBounded attachContextMode = "bounded_task"
	attachContextFull    attachContextMode = "full_task"
)

type attachWorkspaceSource string

const (
	attachWorkspaceRequest attachWorkspaceSource = "request_or_lease"
	attachWorkspaceTask    attachWorkspaceSource = "task_binding"
)

// attachPolicy keeps semantic task association separate from execution
// authority. A weak label/reference may help the model find prior work, but it
// cannot silently change filesystem roots, trust, credentials, or ownership
// of unfinished runs.
type attachPolicy struct {
	ContextMode              attachContextMode
	ExecutionWorkspaceSource attachWorkspaceSource
	ClaimsPriorRuns          bool
}

type taskAttachReason string

const (
	taskAttachNewLabel          taskAttachReason = "new_label"
	taskAttachCurrentPreLabel   taskAttachReason = "current_prelabel"
	taskAttachExplicitTaskID    taskAttachReason = "explicit_task_id"
	taskAttachContinuation      taskAttachReason = "continuation_cue"
	taskAttachResumePin         taskAttachReason = "resume_pin"
	taskAttachReferenceMention  taskAttachReason = "reference_mention"
	taskAttachReferenceContinue taskAttachReason = "reference_continuation"
	// taskAttachApprovalResume binds a daemon-originated approval continuation
	// to the parked approval's origin run — a structured edge, never prose.
	taskAttachApprovalResume taskAttachReason = "approval_resume"
	// taskAttachClarifyResume binds an answered parked clarification to the
	// exact run that asked it. The answer may arrive on any endpoint after a
	// restart, so queue text and task recency are never sufficient authority.
	taskAttachClarifyResume taskAttachReason = "clarify_resume"
	// taskAttachReplyToRun binds a platform-proven reply (threaded IM message,
	// explicit client reply metadata) to the exact run it answers.
	taskAttachReplyToRun taskAttachReason = "reply_to_run"
)

func (a taskAttach) claimsPriorRuns() bool {
	return a.resolvedPolicy().ClaimsPriorRuns
}

func (a taskAttach) selectsPriorRun() bool {
	switch a.reason {
	case taskAttachContinuation, taskAttachExplicitTaskID, taskAttachResumePin:
		return true
	default:
		return false
	}
}

func (a taskAttach) allowsTaskSkillBinding() bool {
	// Exact runtime-owned continuations preserve the already accepted binding.
	// A new interaction or a history reference alone cannot activate it.
	if a.resumesRunID != "" && a.claimsPriorRuns() {
		return true
	}
	switch a.reason {
	case taskAttachExplicitTaskID, taskAttachContinuation, taskAttachResumePin, taskAttachReferenceContinue:
		return true
	default:
		return false
	}
}

func (a taskAttach) resolvedPolicy() attachPolicy {
	if a.policy.ContextMode != "" || a.policy.ExecutionWorkspaceSource != "" {
		return a.policy
	}
	// Compatibility for durable maintenance payloads and focused tests created
	// before policy became explicit. New ingress attaches use newTaskAttach.
	return policyForTaskAttach(a.reason, a.created, a.preLabel)
}

func policyForTaskAttach(reason taskAttachReason, created, preLabel bool) attachPolicy {
	switch reason {
	case taskAttachContinuation, taskAttachApprovalResume, taskAttachClarifyResume, taskAttachReplyToRun:
		return attachPolicy{ContextMode: attachContextFull, ExecutionWorkspaceSource: attachWorkspaceTask, ClaimsPriorRuns: true}
	case taskAttachReferenceContinue, taskAttachReferenceMention:
		// Legacy reasons kept only so durable maintenance payloads recorded
		// before simplification P2 still resolve a policy. References no
		// longer route, load full context, or move the current-task pointer.
		return attachPolicy{ContextMode: attachContextBounded, ExecutionWorkspaceSource: attachWorkspaceRequest}
	case taskAttachExplicitTaskID, taskAttachResumePin:
		return attachPolicy{ContextMode: attachContextFull, ExecutionWorkspaceSource: attachWorkspaceTask, ClaimsPriorRuns: true}
	case taskAttachCurrentPreLabel:
		// Legacy reason (pre-P2 sticky pre-label); replay compatibility only.
		return attachPolicy{ContextMode: attachContextNone, ExecutionWorkspaceSource: attachWorkspaceRequest}
	case taskAttachNewLabel:
		return attachPolicy{ContextMode: attachContextNone, ExecutionWorkspaceSource: attachWorkspaceRequest}
	default:
		if preLabel {
			return attachPolicy{ContextMode: attachContextNone, ExecutionWorkspaceSource: attachWorkspaceRequest}
		}
		return attachPolicy{ContextMode: attachContextFull, ExecutionWorkspaceSource: attachWorkspaceTask}
	}
}

func newTaskAttach(reason taskAttachReason, workKey string, created, preLabel bool) taskAttach {
	return taskAttach{
		created:  created,
		preLabel: preLabel,
		reason:   reason,
		workKey:  workKey,
		policy:   policyForTaskAttach(reason, created, preLabel),
	}
}

// clarifyFallbackSentinel is returned when a question goes unanswered (timeout
// or the run's ctx is done): the agent falls back to its own best judgment
// instead of hanging, so behavior stays safe even when nobody replies. It is
// the SAME instruction the old stub returned, so the unanswered path is
// unchanged from the model's perspective.
const clarifyFallbackSentinel = "No answer arrived in time (the user may be away). Do not guess or continue with an assumption. Finish the run as waiting_user, preserve the question in next_steps, and let a later reply resume the task."

// clarifyWaitTimeout bounds how long a run blocks on a pending question — the
// same 30-minute bound approvals use.
const clarifyWaitTimeout = 30 * time.Minute

// approvalActionTarget derives a compact single-string object of the pending
// action (a path, command, pattern, or name) from the tool args, redacted like
// the stored args. One-line UI surfaces use it; the full redacted args stay on
// the approval row.
func approvalActionTarget(toolName string, args map[string]interface{}) string {
	if toolName == "execute_code" {
		language, _ := args["language"].(string)
		if strings.TrimSpace(language) == "" {
			language = "python"
		}
		return strings.TrimSpace(language) + " script"
	}
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

// personSettingApprovalSurface chooses whether CLI-origin approvals start at
// the desk (default) or are mirrored immediately to the preferred IM. It is
// deliberately separate from notify_platform, which chooses the destination.
const personSettingApprovalSurface = "approval_surface"

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
	//
	// The answer MENU comes from the same server-issued decision list the
	// terminal panel renders (batch B1), so a person answering on WeChat is
	// choosing from the same options — including "don't ask again for commands
	// that start with `git status`" — instead of a poorer two-way yes/no.
	var sb strings.Builder
	sb.WriteString("Approval needed — reply y or n:\n")
	sb.WriteString(approvalSummaryLine(approval, ""))
	if payload := decodeApprovalPayload(approval); payload.TriageRationale != "" {
		sb.WriteString("\nWhy you are being asked: " + payload.TriageRationale)
	}
	if lines := approvalOptionLines(decodeApprovalDecisions(approval.Payload)); lines != "" {
		sb.WriteString("\nOptions:\n" + strings.TrimRight(lines, "\n"))
	}
	return sb.String()
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
	return d.resolveRequestIdentity(r,
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

// writeJSONFlushed commits a complete, length-delimited response to the
// transport before returning. Lifecycle endpoints use it when their successful
// response can immediately cause the owning HTTP server to close.
func writeJSONFlushed(w http.ResponseWriter, status int, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
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
