package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ApprovalRequest struct {
	ID               string          `json:"id"`
	TenantID         string          `json:"tenant_id"`
	PersonID         string          `json:"person_id"`
	TaskID           string          `json:"task_id,omitempty"`
	RunID            string          `json:"run_id,omitempty"`
	ActionType       string          `json:"action_type"`
	Payload          json.RawMessage `json:"payload_json,omitempty"`
	Status           string          `json:"status"`
	RequestedChannel string          `json:"requested_channel,omitempty"`
	ApprovedChannel  string          `json:"approved_channel,omitempty"`
	// DecisionScope carries how long an approval should be remembered: ""
	// (this call only), "task", or "person". It is set when the decision is
	// recorded and read back by the approval waiter to drive class-level
	// approval grants. Empty for pending rows and for reject/expire.
	DecisionScope string `json:"decision_scope,omitempty"`
	// DecisionID is the exact server-issued answer selected for this request
	// (once/task/person/rule:*/deny). Audit must not infer it from scope: a
	// once-only approval legitimately has an empty scope.
	DecisionID string `json:"decision_id,omitempty"`
	// DecisionGrantKey is the narrow RULE the person chose instead of the action
	// class ("commands that start with `git status`"). It is stored verbatim and
	// re-validated by the execution layer against the candidates the SAME call
	// offered, so a decision arriving from any surface cannot mint an
	// authorization the daemon never proposed.
	DecisionGrantKey string `json:"decision_grant_key,omitempty"`
	// DecisionNote is the person's own words when they refused ("use the staging
	// bucket instead"). A rejection with a reason is worth far more to the model
	// than a bare no, and it must be stored rather than only steered, so any
	// endpoint that reads the row later sees why.
	DecisionNote string    `json:"decision_note,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (s *Store) CreateApprovalRequest(ctx context.Context, req ApprovalRequest) (*ApprovalRequest, error) {
	req.TenantID = normalizeTenant(req.TenantID)
	if req.PersonID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	req.ActionType = normalizeName(req.ActionType, "tool_call")
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	if req.ID == "" {
		req.ID = "apr_" + uuid.NewString()
	}
	now := time.Now()
	req.CreatedAt = now
	req.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approval_requests
		   (id, tenant_id, person_id, task_id, run_id, action_type, payload_json, status, requested_channel, approved_channel, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.TenantID, req.PersonID, req.TaskID, req.RunID, req.ActionType, string(req.Payload), req.Status,
		req.RequestedChannel, req.ApprovedChannel, req.CreatedAt.Unix(), req.UpdatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Store) ListApprovalRequests(ctx context.Context, tenantID, personID, status string, limit int) ([]ApprovalRequest, error) {
	if personID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	tenantID = normalizeTenant(tenantID)
	status = normalizeName(status, "pending")
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), action_type,
		        COALESCE(payload_json, '{}'), status, COALESCE(requested_channel, ''), COALESCE(approved_channel, ''),
		        COALESCE(decision_scope, ''), COALESCE(decision_id, ''), COALESCE(decision_grant_key, ''), COALESCE(decision_note, ''), created_at, updated_at
		 FROM approval_requests
		 WHERE tenant_id = ? AND person_id = ? AND status = ?
		 ORDER BY created_at DESC, id DESC LIMIT ?`,
		tenantID, personID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		item, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetApprovalRequest(ctx context.Context, tenantID, approvalID string) (*ApprovalRequest, error) {
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return nil, fmt.Errorf("approval id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), action_type,
		        COALESCE(payload_json, '{}'), status, COALESCE(requested_channel, ''), COALESCE(approved_channel, ''),
		        COALESCE(decision_scope, ''), COALESCE(decision_id, ''), COALESCE(decision_grant_key, ''), COALESCE(decision_note, ''), created_at, updated_at
		 FROM approval_requests WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), approvalID)
	item, err := scanApprovalRequest(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ExpireApprovalRequest finalizes a pending approval whose waiter is gone
// (run cancelled/interrupted/timed out). A stale 'pending' row poisons every
// later interaction: bare y/n turns ambiguous and ordinals hit dead requests.
func (s *Store) ExpireApprovalRequest(ctx context.Context, tenantID, approvalID, reason string) error {
	_ = reason // schema keeps no reason column; parameter documents intent at call sites
	_, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = 'expired', updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'pending'`,
		time.Now().Unix(), tenantID, approvalID)
	return err
}

// ExpireOrphanedApprovals expires every pending approval whose run is no
// longer 'running' — the sweep backstop for waiters that died without
// cleaning up (daemon kill mid-wait). Approvals with no run id are left alone.
func (s *Store) ExpireOrphanedApprovals(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = 'expired', updated_at = ?
		 WHERE status = 'pending' AND COALESCE(run_id, '') != ''
		   AND NOT EXISTS (SELECT 1 FROM task_runs r WHERE r.id = approval_requests.run_id AND r.status = 'running')`,
		time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkApprovalNotified stamps notified_at on a pending approval once an IM
// notification has actually been SENT for it (initial detached push or escrow
// re-push). Idempotent: only a still-pending row with no prior stamp is touched,
// so a crashed push retries next sweep and a resolved approval is never marked.
func (s *Store) MarkApprovalNotified(ctx context.Context, tenantID, approvalID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests SET notified_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'pending' AND notified_at IS NULL`,
		time.Now().Unix(), normalizeTenant(tenantID), approvalID)
	return err
}

// ListPendingApprovalsForEscrow returns pending approvals created at or before
// createdBefore that have not yet had an IM notification sent (notified_at IS
// NULL), across all tenants (the sweep is daemon-wide). Oldest first so the
// person hears about the longest-waiting approval first.
func (s *Store) ListPendingApprovalsForEscrow(ctx context.Context, createdBefore time.Time) ([]ApprovalRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), action_type,
		        COALESCE(payload_json, '{}'), status, COALESCE(requested_channel, ''), COALESCE(approved_channel, ''),
		        COALESCE(decision_scope, ''), COALESCE(decision_id, ''), COALESCE(decision_grant_key, ''), COALESCE(decision_note, ''), created_at, updated_at
		 FROM approval_requests
		 WHERE status = 'pending' AND notified_at IS NULL AND created_at <= ?
		 ORDER BY created_at ASC, id ASC LIMIT 100`,
		createdBefore.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		item, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RespondApprovalRequest records a decision. grantScope (""/"task"/"person")
// is persisted only for an approval and drives class-level approval memory; it
// is ignored for a rejection.
// ApprovalDecisionInput carries the optional parts of a decision beyond
// approve/reject: how long to remember it, WHICH narrow rule the person picked,
// and their words when refusing. It is a struct rather than more positional
// parameters because every caller sets a different subset.
type ApprovalDecisionInput struct {
	GrantScope string
	DecisionID string
	// GrantKey is a rule key the surface offered with this ask. The store keeps
	// it verbatim; the execution layer decides whether it is honored.
	GrantKey string
	// Note is the person's refusal guidance, stored so any endpoint reading the
	// row later sees why the action was refused.
	Note string
}

func (s *Store) RespondApprovalRequest(ctx context.Context, tenantID, personID, approvalID, decision, channel string, input ApprovalDecisionInput) (*ApprovalRequest, error) {
	tenantID = normalizeTenant(tenantID)
	if personID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return nil, fmt.Errorf("approval id is required")
	}
	status, err := normalizeApprovalDecision(decision)
	if err != nil {
		return nil, err
	}
	grantScope := normalizeGrantScope(input.GrantScope)
	decisionID := strings.TrimSpace(input.DecisionID)
	grantKey := strings.TrimSpace(input.GrantKey)
	note := strings.TrimSpace(input.Note)
	if status != "approved" {
		// A grant only makes sense on approval; a note only on refusal. Keeping
		// the pairing here means no caller can store a rule alongside a rejection.
		grantScope, grantKey = "", ""
		if decisionID == "" {
			decisionID = "deny"
		}
	} else {
		note = ""
		if decisionID == "" {
			switch {
			case grantKey != "":
				decisionID = "rule"
			case grantScope == "run":
				decisionID = "run"
			case grantScope == "task":
				decisionID = "task"
			case grantScope == "person":
				decisionID = "person"
			default:
				decisionID = "once"
			}
		}
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests
		 SET status = ?, approved_channel = ?, decision_scope = ?, decision_id = ?, decision_grant_key = ?, decision_note = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending'`,
		status, channel, grantScope, decisionID, grantKey, note, time.Now().Unix(), tenantID, personID, approvalID)
	if err != nil {
		return nil, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		existing, err := s.GetApprovalRequest(ctx, tenantID, approvalID)
		if err != nil {
			return nil, err
		}
		if existing == nil || existing.PersonID != personID {
			return nil, fmt.Errorf("approval request not found: %s", approvalID)
		}
		if existing.Status != "pending" {
			return nil, fmt.Errorf("approval request %s is already %s", approvalID, existing.Status)
		}
	}
	return s.GetApprovalRequest(ctx, tenantID, approvalID)
}

// normalizeGrantScope maps free-form scope input to the exact decision scope.
// "run" is audit-only here: transient run grants live in the execution scope,
// never in the durable approval_grants table.
func normalizeGrantScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "run":
		return "run"
	case "task", "session":
		return "task"
	case "person", "always", "persistent":
		return "person"
	default:
		return ""
	}
}

func normalizeApprovalDecision(decision string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approve", "approved", "yes", "allow", "ok":
		return "approved", nil
	case "reject", "rejected", "deny", "denied", "no":
		return "rejected", nil
	default:
		return "", fmt.Errorf("approval decision must be approved or rejected")
	}
}

func scanApprovalRequest(rows interface {
	Scan(dest ...interface{}) error
}) (ApprovalRequest, error) {
	var item ApprovalRequest
	var payload string
	var created, updated int64
	if err := rows.Scan(&item.ID, &item.TenantID, &item.PersonID, &item.TaskID, &item.RunID, &item.ActionType,
		&payload, &item.Status, &item.RequestedChannel, &item.ApprovedChannel, &item.DecisionScope, &item.DecisionID,
		&item.DecisionGrantKey, &item.DecisionNote, &created, &updated); err != nil {
		return ApprovalRequest{}, err
	}
	item.Payload = json.RawMessage(payload)
	item.CreatedAt = time.Unix(created, 0)
	item.UpdatedAt = time.Unix(updated, 0)
	return item, nil
}
