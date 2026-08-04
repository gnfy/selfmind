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
	// RunGrantClass describes the only non-persistent reuse option available for
	// an unclassifiable exec payload: the byte-identical action in this live run.
	// It never carries the command and can never be promoted to task/person scope.
	RunGrantClass string `json:"run_grant_class,omitempty"`
	// ResourceFingerprint is a non-secret, stable scope for reusable grants.
	// It identifies the workspace and command family, never raw command text,
	// paths, tokens, or credential bytes.
	ResourceFingerprint string `json:"resource_fingerprint,omitempty"`
	// Environment and Cwd say WHERE the operation would run. A decision made
	// without them is a guess: the same command is routine in the workspace root
	// and alarming somewhere else, and the daemon's cwd is not the workspace
	// (docs/tool-safety.md "Execution scope"). Both are display-only context;
	// authorization still comes from the scope that produced them.
	Environment string `json:"environment,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	// ChangeSummary is a compact, content-free description of what a write would
	// do ("2 files +48/-12", "120 lines, 3.4 KB"). It carries counts only — never
	// file content or diff text — so an approval surface can show the SIZE of a
	// change without becoming a content channel.
	ChangeSummary string `json:"change_summary,omitempty"`
	// RuleCandidates are the narrow authorizations this call could create — a
	// command prefix, a network host, a writable root. The surface renders them
	// as answer options; a decision may name at most one, and only one of these
	// (see approvalRuleByKey). Empty means nothing narrower than the action class
	// can be described for this call.
	RuleCandidates []ApprovalRuleCandidate `json:"rule_candidates,omitempty"`
	// TriageRationale is the judge's one-line reasoning when automatic triage ran
	// and handed the call to a human. It is shown at decision time so the person
	// inherits the judgement instead of starting from scratch, and it is stored
	// with the request so a later reader can audit why the ask happened.
	TriageRationale string `json:"triage_rationale,omitempty"`
	// TriageRisk and TriageAuthorization are the judge's structured assessment:
	// how risky the action is, and how directly the person's own words authorize
	// it. They are separate axes on purpose — a low-risk action nobody asked for
	// still deserves a human, and that distinction is invisible in a single
	// verdict word.
	TriageRisk          string `json:"triage_risk,omitempty"`
	TriageAuthorization string `json:"triage_authorization,omitempty"`
	// TriageState explains why a human is being asked in smart mode:
	// TriageStateUnavailable means automatic triage could not rule on this call
	// (no judge configured, or the judge errored/timed out), so the ask is a
	// fail-safe fallback rather than a considered escalation. Empty means the
	// mode simply asks for this class. It exists because "why am I being asked so
	// much?" was unanswerable from any surface.
	TriageState string `json:"triage_state,omitempty"`
	// Containment is the non-secret three-axis execution view supplied to the
	// judge and approval surfaces. It is context, never an authorization.
	Containment string `json:"containment,omitempty"`
}

// TriageStateUnavailable marks an ask that happened because smart-mode triage
// was unavailable (missing judge, error, or timeout) rather than because the
// judge deliberately escalated.
const TriageStateUnavailable = "unavailable"

// TriageStateEscalated marks an ask the judge deliberately handed to the human.
const TriageStateEscalated = "escalated"

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
	// only), "run" (in-memory for the live run), "task" (grant the action's class
	// for the current task) or "person" (grant it for the person across tasks).
	// It drives class-level approval
	// memory in SmartApprovalMiddleware — the key approval-fatigue reducer.
	Scope string `json:"scope,omitempty"`
	// GrantKey names a RULE the human chose instead of the action class ("don't
	// ask again for commands that start with `git status`"). It is honored only
	// when it matches a candidate the SAME call offered, so a decision can never
	// introduce an authorization the daemon did not propose.
	GrantKey string `json:"grant_key,omitempty"`
	// Outcome distinguishes an unanswered approval from a refused one. Both stop
	// the call, but a rejection is a decision (never retry a variant) while a
	// timeout means nobody was there — the model should park the work instead.
	Outcome string `json:"outcome,omitempty"`
}

// Approval outcomes. Empty means "approved" for compatibility with callers that
// only set Approved.
const (
	ApprovalOutcomeApproved = "approved"
	ApprovalOutcomeDenied   = "denied"
	ApprovalOutcomeTimedOut = "timed_out"
)

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
