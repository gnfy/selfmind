package api

import (
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
)

const (
	LocalControlTokenHeader        = "X-SelfMind-Local-Control-Token"
	ShutdownReasonServiceReconcile = "service_reconcile"
	ModelControlProtocolVersion    = 2
)

type ActiveRunStatus struct {
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	TaskID         string `json:"task_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Channel        string `json:"channel,omitempty"`
	Summary        string `json:"summary,omitempty"`
	StartedAt      string `json:"started_at"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
}

type GatewayRuntimeInfo struct {
	PID                   int    `json:"pid"`
	InstanceID            string `json:"instance_id,omitempty"`
	Addr                  string `json:"addr"`
	DataDir               string `json:"data_dir,omitempty"`
	RuntimeDir            string `json:"runtime_dir,omitempty"`
	State                 string `json:"state"`
	StartedAt             string `json:"started_at,omitempty"`
	UpdatedAt             string `json:"updated_at,omitempty"`
	HeartbeatAt           string `json:"heartbeat_at,omitempty"`
	ExitReason            string `json:"exit_reason,omitempty"`
	DefaultTenantID       string `json:"default_tenant_id,omitempty"`
	Version               string `json:"version,omitempty"`
	Commit                string `json:"commit,omitempty"`
	BuiltAt               string `json:"built_at,omitempty"`
	BuildFingerprint      string `json:"build_fingerprint,omitempty"`
	ConfigPath            string `json:"config_path,omitempty"`
	ServiceManager        string `json:"service_manager,omitempty"`
	ServiceGeneration     string `json:"service_generation,omitempty"`
	ModelRouteFingerprint string `json:"model_route_fingerprint,omitempty"`
	// Environment identity of the RUNNING daemon. `env refresh` must compare a
	// fresh login-shell sample against THIS, not against the CLI's own
	// environment: the CLI is usually the first process to see a new toolchain,
	// so comparing against itself reported "unchanged" precisely when the daemon
	// was the stale one. Fingerprints only — never variable names or values.
	EnvironmentGeneration  int64  `json:"environment_generation,omitempty"`
	EnvironmentSnapshotID  string `json:"environment_snapshot_id,omitempty"`
	PrincipalFingerprint   string `json:"principal_fingerprint,omitempty"`
	EnvironmentFingerprint string `json:"environment_fingerprint,omitempty"`
	CredentialSourceHash   string `json:"credential_source_hash,omitempty"`
}

type GatewayStatusResponse struct {
	Runtime        GatewayRuntimeInfo     `json:"runtime"`
	State          string                 `json:"state"`
	Draining       bool                   `json:"draining"`
	DrainReason    string                 `json:"drain_reason,omitempty"`
	ActiveRuns     []ActiveRunStatus      `json:"active_runs"`
	ActiveRunCount int                    `json:"active_run_count"`
	ToolSchemas    ToolSchemaHealth       `json:"tool_schemas,omitempty"`
	ToolCatalog    llm.ToolCatalogPreview `json:"tool_catalog,omitempty"`
	MCP            MCPHealth              `json:"mcp,omitempty"`
	StoreSchema    StoreSchemaHealth      `json:"store_schema,omitempty"`
}

type ProviderToolCatalogProbeResponse struct {
	OK        bool                   `json:"ok"`
	Provider  string                 `json:"provider,omitempty"`
	Model     string                 `json:"model,omitempty"`
	LatencyMS int64                  `json:"latency_ms"`
	Catalog   llm.ToolCatalogPreview `json:"catalog"`
	Error     string                 `json:"error,omitempty"`
}

type ModelProbeRequest struct {
	Role string `json:"role"`
}

type ModelProbeResponse struct {
	OK        bool   `json:"ok"`
	Role      string `json:"role"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type ModelChangeRequest struct {
	Action             string                      `json:"action"`
	Route              string                      `json:"route,omitempty"`
	Provider           string                      `json:"provider,omitempty"`
	Model              string                      `json:"model,omitempty"`
	Reasoning          *string                     `json:"reasoning,omitempty"`
	ServiceTier        *string                     `json:"service_tier,omitempty"`
	ChangeID           string                      `json:"change_id,omitempty"`
	ExpectedGeneration int64                       `json:"expected_generation,omitempty"`
	ReplacePending     bool                        `json:"replace_pending,omitempty"`
	Patches            []ModelSelectionPatch       `json:"patches,omitempty"`
	ValidateRoutes     []string                    `json:"validate_routes,omitempty"`
	CredentialStage    string                      `json:"credential_stage,omitempty"`
	ProviderPatches    []modelchange.ProviderPatch `json:"provider_patches,omitempty"`
}

type ModelSelectionPatch struct {
	Route       string  `json:"route"`
	Provider    string  `json:"provider,omitempty"`
	Model       string  `json:"model,omitempty"`
	Reasoning   *string `json:"reasoning,omitempty"`
	ServiceTier *string `json:"service_tier,omitempty"`
	Reset       bool    `json:"reset,omitempty"`
	APIKey      string  `json:"api_key,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

type ModelChangeResponse struct {
	ProtocolVersion  int                       `json:"protocol_version,omitempty"`
	Status           *modelchange.Status       `json:"status,omitempty"`
	Change           *modelchange.Change       `json:"change,omitempty"`
	Notices          []string                  `json:"notices,omitempty"`
	NeedsConfirm     bool                      `json:"needs_confirm,omitempty"`
	NeedsRestart     bool                      `json:"needs_restart,omitempty"`
	Probes           []modelchange.ProbeResult `json:"probes,omitempty"`
	RestartScheduled bool                      `json:"restart_scheduled,omitempty"`
	CredentialStage  string                    `json:"credential_stage,omitempty"`
}

type MCPServerFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type MCPHealth struct {
	Configured int                `json:"configured"`
	Connected  int                `json:"connected"`
	Failed     int                `json:"failed"`
	Failures   []MCPServerFailure `json:"failures,omitempty"`
}

type StoreSchemaHealth struct {
	Version        int  `json:"version"`
	CurrentVersion int  `json:"current_version"`
	BackupCreated  bool `json:"backup_created,omitempty"`
}

type ToolSchemaHealth struct {
	// Active is retained for older thin clients. It has the same value as
	// RegisteredActive and does not mean provider-visible.
	Active           int `json:"active"`
	RegisteredActive int `json:"registered_active"`
	Hidden           int `json:"hidden"`
	ProviderVisible  int `json:"provider_visible"`
	Repaired         int `json:"repaired"`
	Quarantined      int `json:"quarantined"`
}

type MessageRequest struct {
	TenantID       string `json:"tenant_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name"`
	Channel        string `json:"channel"`
	Content        string `json:"content"`
	WorkspaceID    string `json:"workspace_id"`
	ClientCWD      string `json:"client_cwd,omitempty"`
	// ClientAdditionalRoots is the repeatable local CLI --add-dir surface. It
	// names paths on the daemon host and is therefore accepted only from an
	// authenticated loopback CLI request. The gateway canonicalizes these into
	// ExecutionRoots before queueing or starting a run.
	ClientAdditionalRoots []string `json:"client_additional_roots,omitempty"`
	TaskID                string   `json:"task_id"`
	// ReplyToRunID is platform-proven reply metadata: the client or adapter
	// asserts this message is a reply to the output of one specific run
	// (a threaded IM reply, a numbered pick). The gateway validates the run
	// against the resolved person and binds the turn to it as the exact
	// continuation parent; an invalid, foreign, stale, or already-claimed id
	// fails closed and is never downgraded to ordinary routing.
	ReplyToRunID string `json:"reply_to_run_id,omitempty"`
	// ApprovalID and ClarifyID are structured return edges. Adapters may attach
	// them when the platform proves which pending interaction the user answered;
	// the gateway validates person ownership before using either id.
	ApprovalID string `json:"approval_id,omitempty"`
	ClarifyID  string `json:"clarify_id,omitempty"`
	// ChoiceID names a durable pre-run continuity clarification. It is safe to
	// carry across bound endpoints; the gateway still validates person ownership
	// and atomically consumes the choice before applying its selected target.
	ChoiceID    string              `json:"choice_id,omitempty"`
	Async       bool                `json:"async"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	// AllowWeb opts this turn into web tools even though the default policy keeps
	// them off. Used by scheduled jobs that must look things up (e.g. a market
	// summary). It does not force web use; it only makes the tools available.
	AllowWeb bool `json:"allow_web,omitempty"`
	// ApprovalMode is the codex-style approval policy chosen by the client
	// (read-only / auto-edit / full-auto / smart / on-request). Empty = smart.
	ApprovalMode string `json:"approval_mode,omitempty"`
	// QueueID is set ONLY by the coordinator when it drains a queued task into an
	// async run, so the run's finalization can mark that queue row done (and thus
	// never be re-run at the next boot drain). It is internal routing state, never
	// part of the wire request — hence json:"-".
	QueueID string `json:"-"`
	// QueueClaimToken proves that the draining worker owns QueueID's current
	// attempt. It is process-internal and never accepted over the wire.
	QueueClaimToken string `json:"-"`
	// EffectKey deduplicates the logical products of durable system work across
	// retry runs. It is derived from the queue row, never from a client.
	EffectKey string `json:"-"`
	// ExecutionRoots is the gateway-resolved, durable run snapshot. It is never
	// accepted from the wire; queued and recovery paths populate it internally.
	ExecutionRoots []executionenv.RootBinding `json:"-"`
	// AdditionalRootsRequested distinguishes an explicit local --add-dir
	// request from a durable request whose bindings were restored internally.
	// It prevents a continuation from changing an in-flight run's authority.
	AdditionalRootsRequested bool `json:"-"`
	// ExecutionProfile carries an internal execution contract for durable
	// system work. It is never accepted from the wire. Ordinary user turns leave
	// it empty; watcher finalization uses a constrained unattended profile.
	ExecutionProfile string `json:"-"`
	// WatchID identifies internal work materialized from one durable external
	// watcher. It is never accepted from the wire; the queue drain derives it
	// from the stable watcher finalization key so clients can render a concise
	// run boundary without exposing the internal finalization prompt.
	WatchID string `json:"-"`
	// Origin names the initiator of a run the daemon started on the person's
	// behalf ("cron", "watch", and any future background initiator) rather than
	// a turn the person typed at an endpoint, which leaves it empty. It is
	// never accepted from the wire. Clients read it back from `run.started` and
	// render such a run as a result line instead of replaying its progress.
	Origin string `json:"-"`
	// RecoveryMode is trusted queue-derived state for a daemon-owned recovery
	// child. "verify_only" narrows the provider-visible and dispatchable tool
	// surface to read-only observation plus lifecycle tools.
	RecoveryMode string `json:"-"`
	// ForceNew is set only by the gateway's deterministic /new escape hatch or
	// a claimed "this is new work" choice. It is never accepted over the wire.
	ForceNew bool `json:"-"`
	// ContinuityAction records the typed action of an atomically claimed turn
	// choice. In particular, "observe" must finish on the read-only progress
	// path instead of accidentally creating a continuation run.
	ContinuityAction string `json:"-"`
	// ContinuityResolutionID links a durable human choice to the advisory model
	// decision it corrected. It is control-plane metadata, never a wire input.
	ContinuityResolutionID string `json:"-"`
}

// DispatchRequest runs a single management tool on the daemon. It backs
// agent-backed slash commands for thin TUI clients (no in-process agent). The
// server restricts Tool to a management safelist and overrides the tenant scope
// from the resolved identity, so this is not a general tool-execution backdoor.
type DispatchRequest struct {
	TenantID       string                 `json:"tenant_id"`
	Platform       string                 `json:"platform"`
	PlatformUserID string                 `json:"platform_user_id"`
	DisplayName    string                 `json:"display_name"`
	Tool           string                 `json:"tool"`
	Args           map[string]interface{} `json:"args,omitempty"`
}

// DispatchResponse carries the tool's text result.
type DispatchResponse struct {
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

type MessageAttachment struct {
	Kind     string `json:"kind,omitempty"`
	Path     string `json:"path,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

type MessageResponse struct {
	Identity *control.IdentityContext `json:"identity"`
	Task     *control.Task            `json:"task,omitempty"`
	Run      *control.Run             `json:"run,omitempty"`
	Outcome  *RunOutcome              `json:"outcome,omitempty"`
	Turn     *TurnStatus              `json:"turn,omitempty"`
	Context  *ContextBudgetInfo       `json:"context,omitempty"`
	Content  string                   `json:"content"`
	Usage    llm.UsageStats           `json:"usage"`
	Error    string                   `json:"error,omitempty"`
	Accepted bool                     `json:"accepted,omitempty"`
	Choice   *TurnChoice              `json:"choice,omitempty"`
}

// TurnChoice is the structured, cross-endpoint form of a continuity
// clarification. Clients may render the concise text Content, but should
// preserve ID when the platform supports reply metadata.
type TurnChoice struct {
	ID      string             `json:"id"`
	Options []TurnChoiceOption `json:"options"`
}

type TurnChoiceOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type TurnStatus struct {
	Status           string `json:"status"`
	TaskStatus       string `json:"task_status,omitempty"`
	BackgroundStatus string `json:"background_status,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
	RunID            string `json:"run_id,omitempty"`
	// QueueID correlates a turn acknowledgement with the exact daemon Run that
	// will later drain it. Clients must not infer ownership from message text.
	QueueID string `json:"queue_id,omitempty"`
	Message string `json:"message,omitempty"`
}

type ContextBudgetInfo struct {
	TotalChars            int `json:"total_chars,omitempty"`
	WorkspaceChars        int `json:"workspace_chars,omitempty"`
	TaskChars             int `json:"task_chars,omitempty"`
	MemoryChars           int `json:"memory_chars,omitempty"`
	SkillMainBytes        int `json:"skill_main_bytes,omitempty"`
	SkillMainTokens       int `json:"skill_main_tokens,omitempty"`
	SkillCatalogBytes     int `json:"skill_catalog_bytes,omitempty"`
	SkillCatalogTokens    int `json:"skill_catalog_tokens,omitempty"`
	EstimatedInputTokens  int `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens int `json:"estimated_output_tokens,omitempty"`
}

type ApprovalListResponse struct {
	Identity  *control.IdentityContext  `json:"identity"`
	Approvals []control.ApprovalRequest `json:"approvals"`
}

type ApprovalRespondRequest struct {
	TenantID       string `json:"tenant_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name"`
	Channel        string `json:"channel"`
	ApprovalID     string `json:"approval_id"`
	Decision       string `json:"decision"`
	// Scope records approval reuse on an approve. New asks expose only "" (once)
	// or "run" (the live run). task/person remain wire-compatible for historical
	// rows, but a response is rejected unless the server-issued answer list
	// offered that exact scope.
	Scope string `json:"scope,omitempty"`
	// GrantKey names a narrow RULE the person picked from the ask's own
	// server-issued option list ("commands that start with `git status`"). The
	// daemon honors it only if that ask offered it, so a client cannot invent an
	// authorization.
	GrantKey string `json:"grant_key,omitempty"`
	// Note is the person's guidance when refusing, stored with the decision so
	// the reason survives on any endpoint that reads the row later.
	Note string `json:"note,omitempty"`
}

type ApprovalRespondResponse struct {
	Identity *control.IdentityContext `json:"identity"`
	Approval *control.ApprovalRequest `json:"approval"`
}

// RunSteerRequest injects mid-turn user guidance into the caller's active run
// (POST /v1/runs/steer). It exists for thin clients: their process-local
// steering channel can never reach a run executing inside the daemon, so this
// is the only path by which mid-run input reaches the agent loop. Identity
// fields mirror ApprovalRespondRequest; the daemon resolves the platform
// account to a person and steers that person's active run.
type RunSteerRequest struct {
	TenantID       string `json:"tenant_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name"`
	Channel        string `json:"channel"`
	Text           string `json:"text"`
}

// RunSteerResponse acknowledges that guidance was handed to the active run.
type RunSteerResponse struct {
	Identity *control.IdentityContext `json:"identity"`
	Accepted bool                     `json:"accepted"`
}

const RunStatusVerificationPartial = "verification_partial"

type RunOutcome struct {
	Status           string `json:"status"`
	CompletionReason string `json:"completion_reason,omitempty"`
	Resumable        bool   `json:"resumable,omitempty"`
	// RecoveryScheduled means the daemon durably queued one exact-parent child.
	// Recovery is present instead when automatic continuation did not start and
	// the person needs an actionable, person-scoped handoff.
	RecoveryScheduled bool                     `json:"recovery_scheduled,omitempty"`
	Recovery          *control.RecoveryHandoff `json:"recovery,omitempty"`
	Summary           string                   `json:"summary,omitempty"`
	Done              []string                 `json:"done,omitempty"`
	NextSteps         []string                 `json:"next_steps,omitempty"`
	Files             []string                 `json:"files,omitempty"`
	Tests             []string                 `json:"tests,omitempty"`
	Risks             []string                 `json:"risks,omitempty"`
	NeedApprove       bool                     `json:"need_approve,omitempty"`
	Verification      *VerificationOutcome     `json:"verification,omitempty"`
	// External records the independently observed result of a durable external
	// operation. A failed deployment does not mean the SelfMind finalization run
	// failed: the run may have correctly detected, recorded, and reported that
	// external failure.
	External        *ExternalOutcome `json:"external,omitempty"`
	ClaimMismatches []string         `json:"claim_mismatches,omitempty"`
}

// ExternalOutcome is daemon-derived from a durable watcher. Models do not
// author this field, so consumers can distinguish agent execution quality from
// the business result being observed.
type ExternalOutcome struct {
	WatchID            string `json:"watch_id,omitempty"`
	Status             string `json:"status"`
	CheckerStatus      string `json:"checker_status,omitempty"`
	OperationStatus    string `json:"operation_status,omitempty"`
	VerificationStatus string `json:"verification_status,omitempty"`
	Summary            string `json:"summary,omitempty"`
}

// VerificationOutcome is derived from tool-runtime evidence. It never trusts
// a model's prose claim that tests passed. State is one of not_applicable,
// not_run, stale, passed, failed, partial, or blocked.
type VerificationOutcome struct {
	State            string              `json:"state"`
	Summary          string              `json:"summary,omitempty"`
	LatestMutationAt int64               `json:"latest_mutation_at_unix_nano,omitempty"`
	Checks           []VerificationCheck `json:"checks,omitempty"`
}

type VerificationCheck struct {
	Kind       string `json:"kind,omitempty"`
	Command    string `json:"command,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	StartedAt  int64  `json:"started_at_unix_nano,omitempty"`
	FinishedAt int64  `json:"finished_at_unix_nano,omitempty"`
}

// DigestResponse is the attach digest (GET /v1/digest): a bounded,
// person-scoped summary of what happened since the requesting endpoint's last
// presence (accounts.last_seen_at; a 24h window when never seen). It exists so
// reopening the CLI after being away starts with "while you were away" instead
// of a blank prompt — see docs/identity-continuity.md "Runtime attachment
// model".
type DigestResponse struct {
	Identity *control.IdentityContext `json:"identity,omitempty"`
	// SinceUnix is the anchor the digest was computed from (unix seconds).
	SinceUnix int64 `json:"since_unix"`
	// FinishedTasks completed since the anchor (statuses done/completed).
	FinishedTasks []DigestTask `json:"finished_tasks,omitempty"`
	// DisruptedTasks stopped early since the anchor (statuses failed/interrupted).
	DisruptedTasks []DigestTask `json:"disrupted_tasks,omitempty"`
	// UnresolvedTasks are older task cards that still need continuation. They
	// are point-in-time state, not evidence that a run stopped while the client
	// was away, so clients must present them separately from DisruptedTasks.
	UnresolvedTasks []DigestTask `json:"unresolved_tasks,omitempty"`
	// PendingApprovals is every approval still waiting for the person, in the
	// same stable display order as /approvals (so ordinals keep meaning).
	PendingApprovals []DigestApproval `json:"pending_approvals,omitempty"`
	// PendingClarifies is every question the agent is blocked on waiting for the
	// person to answer (G3). Like approvals it is point-in-time state: still
	// pending is what matters, so it is not anchored on last presence.
	PendingClarifies []DigestClarify `json:"pending_clarifies,omitempty"`
	// UnconfirmedPushes are outbound IM messages that may never have reached
	// the person (status sent_unconfirmed or failed) since the anchor.
	UnconfirmedPushes []DigestPush `json:"unconfirmed_pushes,omitempty"`
	// ActiveRun is the person's currently executing run, if any — the signal
	// for a client to re-attach to its live events.
	ActiveRun *DigestActiveRun `json:"active_run,omitempty"`
	// ApprovalMode is the person's effective approval mode (their persisted
	// /mode preference, or "on-request" when unset). It is point-in-time person
	// state — NOT a "while you were away" item — so it never affects Empty();
	// clients read it to show the current mode in the status bar from startup.
	ApprovalMode string `json:"approval_mode,omitempty"`
}

// Empty reports whether there is literally nothing to tell the user; clients
// must render nothing in that case (a fresh session stays clean).
func (d *DigestResponse) Empty() bool {
	return d == nil ||
		(len(d.FinishedTasks) == 0 && len(d.DisruptedTasks) == 0 &&
			len(d.UnresolvedTasks) == 0 &&
			len(d.PendingApprovals) == 0 && len(d.PendingClarifies) == 0 &&
			len(d.UnconfirmedPushes) == 0 && d.ActiveRun == nil)
}

type DigestTask struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

// ApprovalDecision is one server-issued answer. Clients render this list and
// return its opaque fields unchanged; they never invent authorization scope.
type ApprovalDecision struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Decision  string `json:"decision"`
	Scope     string `json:"scope,omitempty"`
	GrantKey  string `json:"grant_key,omitempty"`
	RuleLabel string `json:"rule_label,omitempty"`
	Key       string `json:"key,omitempty"`
}

// DigestApproval carries the complete approval prompt needed to rebuild the
// same interactive panel after re-attach. Line remains the compact digest/list
// summary; the structured fields are authoritative for the panel.
type DigestApproval struct {
	ID            string             `json:"id"`
	Line          string             `json:"line"`
	WaiterState   string             `json:"waiter_state,omitempty"`
	Tool          string             `json:"tool,omitempty"`
	Target        string             `json:"target,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	Environment   string             `json:"environment,omitempty"`
	Cwd           string             `json:"cwd,omitempty"`
	ChangeSummary string             `json:"change_summary,omitempty"`
	GrantClass    string             `json:"grant_class,omitempty"`
	Containment   string             `json:"containment,omitempty"`
	TriageState   string             `json:"triage_state,omitempty"`
	Rationale     string             `json:"triage_rationale,omitempty"`
	Risk          string             `json:"triage_risk,omitempty"`
	CodePreview   string             `json:"code_preview,omitempty"`
	CodeSHA256    string             `json:"code_sha256,omitempty"`
	CodeLines     int                `json:"code_lines,omitempty"`
	CodeBytes     int                `json:"code_bytes,omitempty"`
	Decisions     []ApprovalDecision `json:"decisions,omitempty"`
}

// DigestClarify carries a pending question's id plus its one-line summary;
// clients show the line and the person answers by replying (the gateway routes
// the reply to the waiting run).
type DigestClarify struct {
	ID   string `json:"id"`
	Line string `json:"line"`
}

type DigestPush struct {
	Platform string `json:"platform"`
	Status   string `json:"status"`
	Preview  string `json:"preview,omitempty"`
}

// DigestActiveRun tells a re-attaching client not just THAT a run is
// executing but WHERE it stands: the same update_plan checklist /status
// renders (bounded, pre-rendered lines) plus the latest progress event, so
// reopening the CLI answers "how far along is it?" at a glance.
type DigestActiveRun struct {
	RunID          string `json:"run_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	Title          string `json:"title,omitempty"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
	// PlanSteps is the run's current plan as pre-rendered checklist lines
	// ("✓ done", "→ current", "○ pending", "− cancelled"), bounded to
	// a handful of lines server-side; a long plan collapses completed leading
	// steps and truncates the tail with "… N more steps". Empty when the run
	// published no plan.
	PlanSteps []string `json:"plan_steps,omitempty"`
	// LatestActivity is ONE line describing the most recent progress event
	// (tool call or thinking note), the "what is it doing right now" signal.
	LatestActivity string `json:"latest_activity,omitempty"`
}

type WorkspaceRegisterRequest struct {
	TenantID       string   `json:"tenant_id"`
	Platform       string   `json:"platform"`
	PlatformUserID string   `json:"platform_user_id"`
	DisplayName    string   `json:"display_name"`
	Name           string   `json:"name"`
	RepoURL        string   `json:"repo_url"`
	LocalPath      string   `json:"local_path"`
	DefaultBranch  string   `json:"default_branch"`
	AllowedRoots   []string `json:"allowed_roots"`
}

type WorkspaceTrustRequest struct {
	TenantID       string `json:"tenant_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	WorkspaceID    string `json:"workspace_id"`
	TrustLevel     string `json:"trust_level"`
}

type WorkspaceCapabilityRequest struct {
	TenantID       string `json:"tenant_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	WorkspaceID    string `json:"workspace_id"`
	Capability     string `json:"capability,omitempty"`
}

type WorkspaceCapability struct {
	Capability string    `json:"capability"`
	GrantedBy  string    `json:"granted_by,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// WorkspaceObservationProfileRequest is accepted only from the authenticated
// local CLI. It records a hash-bound assertion that an unchanged workspace
// script is read-only for the declared argv and environment shape.
type WorkspaceObservationProfileRequest struct {
	TenantID         string   `json:"tenant_id"`
	Platform         string   `json:"platform"`
	PlatformUserID   string   `json:"platform_user_id"`
	WorkspaceID      string   `json:"workspace_id,omitempty"`
	ScriptPath       string   `json:"script_path"`
	ArgvPrefix       []string `json:"argv_prefix,omitempty"`
	AllowTrailing    bool     `json:"allow_trailing,omitempty"`
	AllowNetwork     bool     `json:"allow_network,omitempty"`
	AllowCredentials bool     `json:"allow_credentials,omitempty"`
}

type BindAccountRequest struct {
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name"`
}

// SessionSummary is one indexed working session (FTS row) returned by the
// daemon session APIs. Mirrors kernel/memory.FTS5Session without importing it
// so the API package stays decoupled from kernel storage types.
type SessionSummary struct {
	SessionID string `json:"session_id"`
	Channel   string `json:"channel"`
	Content   string `json:"content,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// SessionMessage is a single indexed message inside a historical session.
type SessionMessage struct {
	SessionID string `json:"session_id"`
	MessageID int    `json:"message_id"`
	Channel   string `json:"channel"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
}

// SessionsResponse carries session search / recent-session results.
type SessionsResponse struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionMessagesResponse carries one session's message window.
type SessionMessagesResponse struct {
	Messages []SessionMessage `json:"messages"`
}
