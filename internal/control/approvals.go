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
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
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
		        created_at, updated_at
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
		        created_at, updated_at
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

func (s *Store) RespondApprovalRequest(ctx context.Context, tenantID, personID, approvalID, decision, channel string) (*ApprovalRequest, error) {
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
	result, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests
		 SET status = ?, approved_channel = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending'`,
		status, channel, time.Now().Unix(), tenantID, personID, approvalID)
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
		&payload, &item.Status, &item.RequestedChannel, &item.ApprovedChannel, &created, &updated); err != nil {
		return ApprovalRequest{}, err
	}
	item.Payload = json.RawMessage(payload)
	item.CreatedAt = time.Unix(created, 0)
	item.UpdatedAt = time.Unix(updated, 0)
	return item, nil
}
