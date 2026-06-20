package tools

import "context"

type ToolApprovalRequest struct {
	TenantID string                 `json:"tenant_id"`
	PersonID string                 `json:"person_id"`
	TaskID   string                 `json:"task_id,omitempty"`
	RunID    string                 `json:"run_id,omitempty"`
	Channel  string                 `json:"channel,omitempty"`
	ToolName string                 `json:"tool_name"`
	Reason   string                 `json:"reason"`
	Args     map[string]interface{} `json:"args,omitempty"`
}

type ToolApprovalDecision struct {
	Approved   bool   `json:"approved"`
	ApprovalID string `json:"approval_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type ToolApprovalHandler func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error)
