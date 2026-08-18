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
	DecisionNote string `json:"decision_note,omitempty"`
	// DecisionRecordedAt is set by versions that implement crash-safe approval
	// continuation. Older terminal rows intentionally keep it NULL after schema
	// migration, so startup recovery cannot reinterpret historical approvals as
	// decisions committed in the current decision->waiter crash window.
	DecisionRecordedAt *time.Time `json:"decision_recorded_at,omitempty"`
	// WaiterState is live while the original tool call is blocked, parked after
	// that run releases resources, and claimed once a live waiter consumes the
	// recorded decision. It is independent from Status, which records the human
	// decision lifecycle.
	WaiterState              string     `json:"waiter_state,omitempty"`
	ParkedAt                 *time.Time `json:"parked_at,omitempty"`
	ParkReason               string     `json:"park_reason,omitempty"`
	DecisionClaimedAt        *time.Time `json:"decision_claimed_at,omitempty"`
	ClaimedByRunID           string     `json:"claimed_by_run_id,omitempty"`
	ResumeQueueID            string     `json:"resume_queue_id,omitempty"`
	AuthorizationFingerprint string     `json:"-"`
	AuthorizationState       string     `json:"-"`
	AuthorizationExpiresAt   *time.Time `json:"-"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

// ApprovalBacklogStats is a current stock measurement, unlike approval.* event
// counts which describe transitions inside a reporting window.
type ApprovalBacklogStats struct {
	Live           int
	Parked         int
	OldestParkedAt *time.Time
}

const approvalSelectColumns = `id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), action_type,
	COALESCE(payload_json, '{}'), status, COALESCE(requested_channel, ''), COALESCE(approved_channel, ''),
	COALESCE(decision_scope, ''), COALESCE(decision_id, ''), COALESCE(decision_grant_key, ''), COALESCE(decision_note, ''),
	COALESCE(decision_recorded_at, 0),
	COALESCE(waiter_state, 'live'), COALESCE(parked_at, 0), COALESCE(park_reason, ''),
	COALESCE(decision_claimed_at, 0), COALESCE(claimed_by_run_id, ''), COALESCE(resume_queue_id, ''),
	COALESCE(authorization_fingerprint, ''), COALESCE(authorization_state, ''), COALESCE(authorization_expires_at, 0),
	created_at, updated_at`

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
	if req.WaiterState == "" {
		req.WaiterState = "live"
	}
	if req.ID == "" {
		req.ID = "apr_" + uuid.NewString()
	}
	now := time.Now()
	req.CreatedAt = now
	req.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approval_requests
		   (id, tenant_id, person_id, task_id, run_id, action_type, payload_json, status, requested_channel, approved_channel,
		    waiter_state, authorization_fingerprint, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.TenantID, req.PersonID, req.TaskID, req.RunID, req.ActionType, string(req.Payload), req.Status,
		req.RequestedChannel, req.ApprovedChannel, req.WaiterState, req.AuthorizationFingerprint,
		req.CreatedAt.Unix(), req.UpdatedAt.Unix())
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
		`SELECT `+approvalSelectColumns+`
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

func (s *Store) ApprovalBacklog(ctx context.Context, tenantID, personID string) (ApprovalBacklogStats, error) {
	if strings.TrimSpace(personID) == "" {
		return ApprovalBacklogStats{}, fmt.Errorf("person id is required")
	}
	var stats ApprovalBacklogStats
	var oldest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN COALESCE(waiter_state, 'live') = 'live' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN waiter_state = 'parked' THEN 1 ELSE 0 END), 0),
		MIN(CASE WHEN waiter_state = 'parked' THEN COALESCE(parked_at, created_at) END)
		FROM approval_requests WHERE tenant_id = ? AND person_id = ? AND status = 'pending'`,
		normalizeTenant(tenantID), strings.TrimSpace(personID)).Scan(&stats.Live, &stats.Parked, &oldest)
	if err != nil {
		return ApprovalBacklogStats{}, err
	}
	if oldest.Valid && oldest.Int64 > 0 {
		value := time.Unix(oldest.Int64, 0)
		stats.OldestParkedAt = &value
	}
	return stats, nil
}

func (s *Store) GetApprovalRequest(ctx context.Context, tenantID, approvalID string) (*ApprovalRequest, error) {
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return nil, fmt.Errorf("approval id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+approvalSelectColumns+`
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

// IsApprovalPending reports whether an approval delivery still represents an
// actionable request for the same person. Delivery workers call this at the
// final network boundary so delayed retries and IM catch-up cannot resurrect
// an approval that was already approved, denied, or expired.
func (s *Store) IsApprovalPending(ctx context.Context, tenantID, personID, approvalID string) (bool, error) {
	approvalID = strings.TrimSpace(approvalID)
	personID = strings.TrimSpace(personID)
	if approvalID == "" || personID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM approval_requests
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending'`,
		normalizeTenant(tenantID), personID, approvalID).Scan(&n)
	return n == 1, err
}

// ExpireApprovalRequest finalizes a pending approval after an explicit cancel
// or administrative invalidation. Resource timeouts and daemon loss use
// ParkApprovalRequest instead, preserving answerability. The returned
// transition flag is the routing signal: callers emit approval.expired only
// when this call actually won pending -> expired. A zero-row update can mean a
// human decision won the race, so the current row is returned instead of being
// guessed at.
func (s *Store) ExpireApprovalRequest(ctx context.Context, tenantID, approvalID, reason string) (*ApprovalRequest, bool, error) {
	_ = reason // the event records the reason; the approval schema has no reason column
	tenantID = normalizeTenant(tenantID)
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return nil, false, fmt.Errorf("approval id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE approval_requests SET status = 'expired', updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'pending'
		 RETURNING `+approvalSelectColumns,
		time.Now().Unix(), tenantID, approvalID)
	item, err := scanApprovalRequest(row)
	if err == nil {
		return &item, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	current, err := s.GetApprovalRequest(ctx, tenantID, approvalID)
	return current, false, err
}

// ParkApprovalRequest releases a live waiter without invalidating the human
// question. The request remains status=pending and answerable from any surface.
func (s *Store) ParkApprovalRequest(ctx context.Context, tenantID, approvalID, reason string) (*ApprovalRequest, bool, error) {
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE approval_requests SET waiter_state = 'parked', parked_at = ?, park_reason = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'pending' AND COALESCE(waiter_state, 'live') = 'live'
		 RETURNING `+approvalSelectColumns,
		now, strings.TrimSpace(reason), now, normalizeTenant(tenantID), strings.TrimSpace(approvalID))
	item, err := scanApprovalRequest(row)
	if err == nil {
		return &item, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	current, err := s.GetApprovalRequest(ctx, tenantID, approvalID)
	return current, false, err
}

// ParkOrphanedApprovals parks every pending live-waiter approval whose run is
// no longer running. Approvals with no run id and already-parked requests are
// left alone. Returning rows lets recovery publish one idempotent lifecycle
// event so clients relabel their still-answerable panels.
func (s *Store) ParkOrphanedApprovals(ctx context.Context) ([]ApprovalRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE approval_requests SET waiter_state = 'parked', parked_at = COALESCE(parked_at, ?),
		 park_reason = CASE WHEN COALESCE(park_reason, '') = '' THEN 'approval waiter was lost during daemon recovery' ELSE park_reason END,
		 updated_at = ?
		 WHERE status = 'pending' AND COALESCE(waiter_state, 'live') = 'live' AND COALESCE(run_id, '') != ''
		   AND NOT EXISTS (SELECT 1 FROM task_runs r WHERE r.id = approval_requests.run_id AND r.status = 'running')
		 RETURNING `+approvalSelectColumns,
		time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parked []ApprovalRequest
	for rows.Next() {
		item, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		parked = append(parked, item)
	}
	return parked, rows.Err()
}

// ClaimApprovalDecision marks that the original live waiter consumed a human
// decision. Recovery uses the absence of this claim to find a decision that was
// durably recorded just before the daemon died.
func (s *Store) ClaimApprovalDecision(ctx context.Context, tenantID, approvalID, runID string) (*ApprovalRequest, bool, error) {
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE approval_requests SET waiter_state = 'claimed', decision_claimed_at = ?, claimed_by_run_id = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND status IN ('approved', 'rejected')
		   AND COALESCE(waiter_state, 'live') = 'live' AND COALESCE(decision_claimed_at, 0) = 0
		 RETURNING `+approvalSelectColumns,
		now, strings.TrimSpace(runID), now, normalizeTenant(tenantID), strings.TrimSpace(approvalID))
	item, err := scanApprovalRequest(row)
	if err == nil {
		return &item, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	current, err := s.GetApprovalRequest(ctx, tenantID, approvalID)
	return current, false, err
}

// ClaimApprovalResumeAuthorization atomically consumes the oldest unexpired
// parked approval matching the regenerated action. The fingerprint was made
// from raw action material but only its digest is persisted.
func (s *Store) ClaimApprovalResumeAuthorization(ctx context.Context, tenantID, personID, taskID, runID, fingerprint string) (approvalID, decisionID, grantKey string, claimed bool, err error) {
	tenantID = normalizeTenant(tenantID)
	personID, taskID, runID, fingerprint = strings.TrimSpace(personID), strings.TrimSpace(taskID), strings.TrimSpace(runID), strings.TrimSpace(fingerprint)
	if personID == "" || taskID == "" || runID == "" || fingerprint == "" {
		return "", "", "", false, nil
	}
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE approval_requests SET authorization_state = 'consumed', decision_claimed_at = ?,
		 claimed_by_run_id = ?, updated_at = ?
		 WHERE id = (
		   SELECT id FROM approval_requests
		   WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status = 'approved'
		     AND COALESCE(waiter_state, '') = 'parked' AND COALESCE(authorization_state, '') = 'available'
		     AND COALESCE(authorization_fingerprint, '') = ?
		     AND COALESCE(authorization_expires_at, 0) >= ?
		   ORDER BY created_at ASC, id ASC LIMIT 1
		 ) AND authorization_state = 'available'
		 RETURNING id, COALESCE(decision_id, ''), COALESCE(decision_grant_key, '')`,
		now, runID, now, tenantID, personID, taskID, fingerprint, now)
	if err := row.Scan(&approvalID, &decisionID, &grantKey); err != nil {
		if err == sql.ErrNoRows {
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	return approvalID, decisionID, grantKey, true, nil
}

// ArchiveStaleParkedApprovals bounds the answerable backlog without confusing
// retention with rejection. It returns transitioned rows so callers can emit
// approval.archived and reconcile every surface.
func (s *Store) ArchiveStaleParkedApprovals(ctx context.Context, retention time.Duration) ([]ApprovalRequest, error) {
	if retention <= 0 {
		retention = ParkedApprovalRetention
	}
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx,
		`UPDATE approval_requests SET status = 'archived', authorization_state = '', updated_at = ?
		 WHERE status = 'pending' AND waiter_state = 'parked' AND COALESCE(parked_at, created_at) <= ?
		 RETURNING `+approvalSelectColumns,
		now, time.Now().Add(-retention).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var archived []ApprovalRequest
	for rows.Next() {
		item, scanErr := scanApprovalRequest(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		archived = append(archived, item)
	}
	return archived, rows.Err()
}

// ListRecoverableApprovalDecisions finds human decisions committed immediately
// before the original live waiter could claim them. A decision already claimed
// by its run does not prove the approved action was unfinished; ordinary run
// recovery owns that case and must not enqueue one continuation per historical
// approval.
func (s *Store) ListRecoverableApprovalDecisions(ctx context.Context, limit int) ([]ApprovalRequest, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalSelectColumns+` FROM approval_requests a
		 WHERE a.status IN ('approved', 'rejected') AND COALESCE(a.resume_queue_id, '') = ''
		   AND COALESCE(a.decision_recorded_at, 0) > 0
		   AND COALESCE(a.waiter_state, 'live') = 'live' AND COALESCE(a.decision_claimed_at, 0) = 0
		   AND COALESCE(a.run_id, '') != ''
		   AND EXISTS (SELECT 1 FROM task_runs r WHERE r.id = a.run_id AND r.status = 'interrupted')
		 ORDER BY a.updated_at ASC, a.id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		item, scanErr := scanApprovalRequest(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// EnqueueRecoveredApprovalContinuation durably repairs the unclaimed
// decision->resume edge after a crash and creates one matching-action token.
func (s *Store) EnqueueRecoveredApprovalContinuation(ctx context.Context, approvalID string, q QueuedTask) (*QueuedTask, bool, error) {
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" || strings.TrimSpace(q.Content) == "" || strings.TrimSpace(q.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("recovered approval continuation requires approval, content, and idempotency key")
	}
	q.TenantID = normalizeTenant(q.TenantID)
	q.Platform = normalizeName(q.Platform, "cli")
	q.Channel = normalizeName(q.Channel, q.Platform)
	q.Class = normalizeQueueClass(q.Class)
	if q.Priority == 0 {
		q.Priority = queuePriorityForClass(q.Class)
	}
	if q.ID == "" {
		q.ID = "queue_" + uuid.NewString()
	}
	q.Status, q.CreatedAt = QueueStatusQueued, time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT status
		 FROM approval_requests a WHERE a.tenant_id = ? AND a.id = ? AND a.person_id = ? AND a.task_id = ?
		   AND COALESCE(a.resume_queue_id, '') = '' AND a.status IN ('approved', 'rejected')
		   AND COALESCE(a.decision_recorded_at, 0) > 0
		   AND COALESCE(a.waiter_state, 'live') = 'live' AND COALESCE(a.decision_claimed_at, 0) = 0
		   AND EXISTS (SELECT 1 FROM task_runs r WHERE r.id = a.run_id AND r.status = 'interrupted')`,
		q.TenantID, approvalID, q.PersonID, q.TaskID).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	notBefore := int64(0)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO task_queue (id, tenant_id, person_id, channel, platform, platform_user_id, content, approval_mode, workspace_id, task_id, idempotency_key, class, priority, not_before, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, idempotency_key) WHERE idempotency_key != '' DO NOTHING`,
		q.ID, q.TenantID, q.PersonID, q.Channel, q.Platform, q.PlatformUserID, q.Content, q.ApprovalMode,
		q.WorkspaceID, q.TaskID, q.IdempotencyKey, q.Class, q.Priority, notBefore, q.Status, q.CreatedAt.Unix())
	if err != nil {
		return nil, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM task_queue WHERE tenant_id = ? AND idempotency_key = ?`, q.TenantID, q.IdempotencyKey).Scan(&q.ID); err != nil {
		return nil, false, err
	}
	authState, authExpires := "", int64(0)
	if status == "approved" {
		authState = "available"
		authExpires = time.Now().Add(ParkedApprovalRetention).Unix()
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE approval_requests SET waiter_state = 'parked', parked_at = COALESCE(parked_at, ?),
		 park_reason = 'daemon restarted before the decision was fully consumed', resume_queue_id = ?,
		 authorization_state = ?, authorization_expires_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND COALESCE(resume_queue_id, '') = ''`,
		time.Now().Unix(), q.ID, authState, authExpires, time.Now().Unix(), q.TenantID, approvalID)
	if err != nil {
		return nil, false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &q, true, nil
}

// AppendApprovalParkedEvent publishes a non-terminal lifecycle transition. It
// does not close the panel: clients keep the request and explain that answering
// it starts a continuation rather than waking a live run.
func (s *Store) AppendApprovalParkedEvent(ctx context.Context, approval *ApprovalRequest, channel, reason string) (*Event, error) {
	if approval == nil || strings.TrimSpace(approval.TaskID) == "" {
		return nil, nil
	}
	data, err := json.Marshal(map[string]string{
		"approval_id": approval.ID,
		"action_type": approval.ActionType,
		"reason":      strings.TrimSpace(reason),
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(channel) == "" {
		channel = approval.RequestedChannel
	}
	return s.AppendEvent(ctx, Event{
		TaskID: approval.TaskID, RunID: approval.RunID, Type: "approval.parked",
		Visibility: "task", Channel: channel, Payload: data,
		IdempotencyKey: "approval:" + approval.ID + ":parked",
	})
}

// AppendApprovalResolutionEvent publishes the durable state transition that
// attached clients use to reconcile approval panels by id. The idempotency key
// makes recovery retries safe; approval rows remain the source of truth.
func (s *Store) AppendApprovalResolutionEvent(ctx context.Context, approval *ApprovalRequest, channel, reason string) (*Event, error) {
	if approval == nil || strings.TrimSpace(approval.TaskID) == "" {
		return nil, nil
	}
	status := strings.TrimSpace(approval.Status)
	switch status {
	case "approved", "rejected", "expired", "archived":
	default:
		return nil, fmt.Errorf("approval %s has non-terminal status %q", approval.ID, status)
	}
	payload := map[string]string{
		"approval_id": approval.ID,
		"action_type": approval.ActionType,
		"decision_id": approval.DecisionID,
		"scope":       approval.DecisionScope,
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		payload["reason"] = reason
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if channel = strings.TrimSpace(channel); channel == "" {
		channel = approval.RequestedChannel
	}
	return s.AppendEvent(ctx, Event{
		TaskID:         approval.TaskID,
		RunID:          approval.RunID,
		Type:           "approval." + status,
		Visibility:     "task",
		Channel:        channel,
		Payload:        data,
		IdempotencyKey: "approval:" + approval.ID + ":" + status,
	})
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
		`SELECT `+approvalSelectColumns+`
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
	status, grantScope, decisionID, grantKey, note, err := normalizeApprovalDecisionValues(decision, input)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests
		 SET status = ?, approved_channel = ?, decision_scope = ?, decision_id = ?, decision_grant_key = ?, decision_note = ?,
		     decision_recorded_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending' AND COALESCE(waiter_state, 'live') = 'live'`,
		status, channel, grantScope, decisionID, grantKey, note, now, now, tenantID, personID, approvalID)
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
		return nil, fmt.Errorf("approval request %s is %s and must be resumed transactionally", approvalID, existing.WaiterState)
	}
	return s.GetApprovalRequest(ctx, tenantID, approvalID)
}

func normalizeApprovalDecisionValues(decision string, input ApprovalDecisionInput) (status, grantScope, decisionID, grantKey, note string, err error) {
	status, err = normalizeApprovalDecision(decision)
	if err != nil {
		return "", "", "", "", "", err
	}
	grantScope = normalizeGrantScope(input.GrantScope)
	decisionID = strings.TrimSpace(input.DecisionID)
	grantKey = strings.TrimSpace(input.GrantKey)
	note = strings.TrimSpace(input.Note)
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
	return status, grantScope, decisionID, grantKey, note, nil
}

const ParkedApprovalRetention = 7 * 24 * time.Hour

// RespondParkedApprovalAndEnqueue records the answer and its task-pinned
// continuation in one SQLite transaction. A crash can therefore produce
// neither "approved but never resumed" nor duplicate resume rows.
func (s *Store) RespondParkedApprovalAndEnqueue(ctx context.Context, tenantID, personID, approvalID, decision, channel string, input ApprovalDecisionInput, q QueuedTask) (*ApprovalRequest, *QueuedTask, error) {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	approvalID = strings.TrimSpace(approvalID)
	status, grantScope, decisionID, grantKey, note, err := normalizeApprovalDecisionValues(decision, input)
	if err != nil {
		return nil, nil, err
	}
	if personID == "" || approvalID == "" || strings.TrimSpace(q.Content) == "" || strings.TrimSpace(q.TaskID) == "" {
		return nil, nil, fmt.Errorf("parked approval resume requires person, approval, task, and content")
	}
	q.TenantID, q.PersonID = tenantID, personID
	q.Platform = normalizeName(q.Platform, "cli")
	q.Channel = normalizeName(q.Channel, q.Platform)
	q.Class = normalizeQueueClass(q.Class)
	if q.Priority == 0 {
		q.Priority = queuePriorityForClass(q.Class)
	}
	if q.ID == "" {
		q.ID = "queue_" + uuid.NewString()
	}
	q.IdempotencyKey = strings.TrimSpace(q.IdempotencyKey)
	if q.IdempotencyKey == "" {
		return nil, nil, fmt.Errorf("parked approval resume requires an idempotency key")
	}
	q.Status = QueueStatusQueued
	q.CreatedAt = time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var waiterState, currentStatus, owner, taskID string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(waiter_state, 'live'), status, person_id, COALESCE(task_id, '')
		 FROM approval_requests WHERE tenant_id = ? AND id = ?`, tenantID, approvalID,
	).Scan(&waiterState, &currentStatus, &owner, &taskID); err != nil {
		return nil, nil, err
	}
	if owner != personID || currentStatus != "pending" || waiterState != "parked" || taskID != q.TaskID {
		return nil, nil, fmt.Errorf("approval request %s is not an answerable parked request", approvalID)
	}
	notBefore := q.NotBefore.Unix()
	if q.NotBefore.IsZero() {
		notBefore = 0
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO task_queue (id, tenant_id, person_id, channel, platform, platform_user_id, content, approval_mode, workspace_id, task_id, idempotency_key, class, priority, not_before, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, idempotency_key) WHERE idempotency_key != '' DO NOTHING`,
		q.ID, q.TenantID, q.PersonID, q.Channel, q.Platform, q.PlatformUserID, q.Content, q.ApprovalMode,
		q.WorkspaceID, q.TaskID, q.IdempotencyKey, q.Class, q.Priority, notBefore, q.Status, q.CreatedAt.Unix())
	if err != nil {
		return nil, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM task_queue WHERE tenant_id = ? AND idempotency_key = ?`, tenantID, q.IdempotencyKey).Scan(&q.ID); err != nil {
		return nil, nil, err
	}
	now := time.Now().Unix()
	authorizationState, authorizationExpiry := "", int64(0)
	if status == "approved" {
		authorizationState = "available"
		authorizationExpiry = time.Now().Add(ParkedApprovalRetention).Unix()
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE approval_requests SET status = ?, approved_channel = ?, decision_scope = ?, decision_id = ?,
		 decision_grant_key = ?, decision_note = ?, resume_queue_id = ?, authorization_state = ?,
		 authorization_expires_at = ?, decision_recorded_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending' AND waiter_state = 'parked'`,
		status, channel, grantScope, decisionID, grantKey, note, q.ID, authorizationState,
		authorizationExpiry, now, now, tenantID, personID, approvalID)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, nil, fmt.Errorf("approval request %s changed while it was being answered", approvalID)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	approval, err := s.GetApprovalRequest(ctx, tenantID, approvalID)
	if err != nil {
		return nil, nil, err
	}
	return approval, &q, nil
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
	var decisionRecordedAt, parkedAt, claimedAt, authorizationExpiresAt, created, updated int64
	if err := rows.Scan(&item.ID, &item.TenantID, &item.PersonID, &item.TaskID, &item.RunID, &item.ActionType,
		&payload, &item.Status, &item.RequestedChannel, &item.ApprovedChannel, &item.DecisionScope, &item.DecisionID,
		&item.DecisionGrantKey, &item.DecisionNote, &decisionRecordedAt, &item.WaiterState, &parkedAt, &item.ParkReason,
		&claimedAt, &item.ClaimedByRunID, &item.ResumeQueueID, &item.AuthorizationFingerprint,
		&item.AuthorizationState, &authorizationExpiresAt, &created, &updated); err != nil {
		return ApprovalRequest{}, err
	}
	item.Payload = json.RawMessage(payload)
	if decisionRecordedAt > 0 {
		value := time.Unix(decisionRecordedAt, 0)
		item.DecisionRecordedAt = &value
	}
	if parkedAt > 0 {
		value := time.Unix(parkedAt, 0)
		item.ParkedAt = &value
	}
	if claimedAt > 0 {
		value := time.Unix(claimedAt, 0)
		item.DecisionClaimedAt = &value
	}
	if authorizationExpiresAt > 0 {
		value := time.Unix(authorizationExpiresAt, 0)
		item.AuthorizationExpiresAt = &value
	}
	item.CreatedAt = time.Unix(created, 0)
	item.UpdatedAt = time.Unix(updated, 0)
	return item, nil
}
