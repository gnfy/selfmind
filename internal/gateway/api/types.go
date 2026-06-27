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
