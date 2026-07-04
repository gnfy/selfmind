package api

import (
	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
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
	PID             int    `json:"pid"`
	Addr            string `json:"addr"`
	DataDir         string `json:"data_dir,omitempty"`
	RuntimeDir      string `json:"runtime_dir,omitempty"`
	State           string `json:"state"`
	StartedAt       string `json:"started_at,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	DefaultTenantID string `json:"default_tenant_id,omitempty"`
}

type GatewayStatusResponse struct {
	Runtime        GatewayRuntimeInfo `json:"runtime"`
	State          string             `json:"state"`
	Draining       bool               `json:"draining"`
	DrainReason    string             `json:"drain_reason,omitempty"`
	ActiveRuns     []ActiveRunStatus  `json:"active_runs"`
	ActiveRunCount int                `json:"active_run_count"`
}

type MessageRequest struct {
	TenantID       string              `json:"tenant_id"`
	Platform       string              `json:"platform"`
	PlatformUserID string              `json:"platform_user_id"`
	DisplayName    string              `json:"display_name"`
	Channel        string              `json:"channel"`
	Content        string              `json:"content"`
	WorkspaceID    string              `json:"workspace_id"`
	ClientCWD      string              `json:"client_cwd,omitempty"`
	TaskID         string              `json:"task_id"`
	Async          bool                `json:"async"`
	Attachments    []MessageAttachment `json:"attachments,omitempty"`
	// AllowWeb opts this turn into web tools even though the default policy keeps
	// them off. Used by scheduled jobs that must look things up (e.g. a market
	// summary). It does not force web use; it only makes the tools available.
	AllowWeb bool `json:"allow_web,omitempty"`
	// ApprovalMode is the codex-style approval policy chosen by the client
	// (read-only / auto-edit / full-auto / on-request). Empty = on-request.
	ApprovalMode string `json:"approval_mode,omitempty"`
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
}

type TurnStatus struct {
	Status           string `json:"status"`
	TaskStatus       string `json:"task_status,omitempty"`
	BackgroundStatus string `json:"background_status,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
	RunID            string `json:"run_id,omitempty"`
	Message          string `json:"message,omitempty"`
}

type ContextBudgetInfo struct {
	TotalChars            int `json:"total_chars,omitempty"`
	WorkspaceChars        int `json:"workspace_chars,omitempty"`
	TaskChars             int `json:"task_chars,omitempty"`
	MemoryChars           int `json:"memory_chars,omitempty"`
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

type RunOutcome struct {
	Status      string   `json:"status"`
	Summary     string   `json:"summary,omitempty"`
	Done        []string `json:"done,omitempty"`
	NextSteps   []string `json:"next_steps,omitempty"`
	Files       []string `json:"files,omitempty"`
	Tests       []string `json:"tests,omitempty"`
	Risks       []string `json:"risks,omitempty"`
	NeedApprove bool     `json:"need_approve,omitempty"`
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
	// PendingApprovals is every approval still waiting for the person, in the
	// same stable display order as /approvals (so ordinals keep meaning).
	PendingApprovals []DigestApproval `json:"pending_approvals,omitempty"`
	// UnconfirmedPushes are outbound IM messages that may never have reached
	// the person (status sent_unconfirmed or failed) since the anchor.
	UnconfirmedPushes []DigestPush `json:"unconfirmed_pushes,omitempty"`
	// ActiveRun is the person's currently executing run, if any — the signal
	// for a client to re-attach to its live events.
	ActiveRun *DigestActiveRun `json:"active_run,omitempty"`
}

// Empty reports whether there is literally nothing to tell the user; clients
// must render nothing in that case (a fresh session stays clean).
func (d *DigestResponse) Empty() bool {
	return d == nil ||
		(len(d.FinishedTasks) == 0 && len(d.DisruptedTasks) == 0 &&
			len(d.PendingApprovals) == 0 && len(d.UnconfirmedPushes) == 0 &&
			d.ActiveRun == nil)
}

type DigestTask struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

// DigestApproval carries the approval id plus the same one-line rich summary
// the /approvals list renders; clients show the line and defer resolution
// (ordinals, y/n) to the shared gateway resolver.
type DigestApproval struct {
	ID   string `json:"id"`
	Line string `json:"line"`
}

type DigestPush struct {
	Platform string `json:"platform"`
	Status   string `json:"status"`
	Preview  string `json:"preview,omitempty"`
}

type DigestActiveRun struct {
	TaskID         string `json:"task_id,omitempty"`
	Title          string `json:"title,omitempty"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
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

type BindAccountRequest struct {
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name"`
}
