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
	// Scope records how long the approval should be remembered: "" (this call
	// only), "task" (grant the action's class for the current task) or "person"
	// (grant it for the person across tasks). It drives class-level approval
	// memory in SmartApprovalMiddleware — the key approval-fatigue reducer.
	Scope string `json:"scope,omitempty"`
}

type ToolApprovalHandler func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error)

// ApprovalGrantStore consults and records class-level approval grants
// (session/persistent) for the approval middleware. It is defined here — not
// imported from the control plane — so internal/tools keeps no control
// dependency; the gateway installs a control-backed implementation on the
// ExecutionScope. IsApprovalGranted reports whether patternKey is already
// approved for the person (any task) or for the given task; GrantApproval
// records a grant with scopeKind "task" (scopeID = task id) or "person"
// (scopeID = person id).
type ApprovalGrantStore interface {
	IsApprovalGranted(ctx context.Context, tenantID, personID, taskID, patternKey string) (bool, error)
	GrantApproval(ctx context.Context, scopeKind, tenantID, personID, scopeID, patternKey string) error
}
