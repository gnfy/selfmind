package tools

import (
	"context"
	"time"
)

type ToolApprovalRequest struct {
	TenantID string                 `json:"tenant_id"`
	PersonID string                 `json:"person_id"`
	TaskID   string                 `json:"task_id,omitempty"`
	RunID    string                 `json:"run_id,omitempty"`
	Channel  string                 `json:"channel,omitempty"`
	ToolName string                 `json:"tool_name"`
	Reason   string                 `json:"reason"`
	Args     map[string]interface{} `json:"args,omitempty"`
	// GrantClass is the human-readable description of what a "remember this"
	// decision would authorize, or empty when the payload is not eligible for a
	// reusable grant at all. It exists so the surface that reports the decision
	// cannot claim something was remembered when the eligibility floor refused
	// to persist it. It carries a class, never a command or a secret.
	GrantClass string `json:"grant_class,omitempty"`
	// ResourceFingerprint is a non-secret, stable scope for reusable grants.
	// It identifies the workspace and command family, never raw command text,
	// paths, tokens, or credential bytes.
	ResourceFingerprint string `json:"resource_fingerprint,omitempty"`
}

type ToolApprovalDecision struct {
	Approved   bool   `json:"approved"`
	ApprovalID string `json:"approval_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// ExpiresAt is when a remembered decision stops applying. The CONTROL PLANE
	// decides it: the execution layer must not choose how long its own
	// authorization lasts, which is what it did while the window was a constant
	// inside the tool middleware. Zero falls back to the execution layer's
	// conservative default.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
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
// (scopeID = person id). expiresAt bounds the grant; a zero value means no
// deadline, which is only appropriate for a class already bounded by a
// narrower scope.
type ApprovalGrantStore interface {
	IsApprovalGranted(ctx context.Context, tenantID, personID, taskID, patternKey string) (bool, error)
	GrantApproval(ctx context.Context, scopeKind, tenantID, personID, scopeID, patternKey string, expiresAt time.Time) error
}
