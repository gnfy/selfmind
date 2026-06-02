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
	TenantID       string `json:"tenant_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name"`
	Channel        string `json:"channel"`
	Content        string `json:"content"`
	WorkspaceID    string `json:"workspace_id"`
	TaskID         string `json:"task_id"`
	Async          bool   `json:"async"`
}

type MessageResponse struct {
	Identity *control.IdentityContext `json:"identity"`
	Task     *control.Task            `json:"task,omitempty"`
	Run      *control.Run             `json:"run,omitempty"`
	Content  string                   `json:"content"`
	Usage    llm.UsageStats           `json:"usage"`
	Error    string                   `json:"error,omitempty"`
	Accepted bool                     `json:"accepted,omitempty"`
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
