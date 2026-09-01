package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrClarifyOriginUnavailable means the question can no longer create its
// promised exact continuation because the origin run is terminal, missing, or
// already owned by another child. The stale pending row is expired before this
// error is returned, so later user input is not trapped by it.
var ErrClarifyOriginUnavailable = errors.New("clarification origin run is no longer available")

// ClarifyRequest is a durable pending question the agent asked the person mid
// run. It is modeled exactly on ApprovalRequest so a question survives the
// endpoint that raised it closing: the run blocks on the DB row (not on a live
// prompt channel), and any endpoint can answer it (docs/identity-continuity.md
// "Runtime attachment model"). Unlike an approval — a bounded yes/no — an
// answer is free text, so there is no decision_scope/grant machinery here.
type ClarifyRequest struct {
	ID       string          `json:"id"`
	TenantID string          `json:"tenant_id"`
	PersonID string          `json:"person_id"`
	TaskID   string          `json:"task_id,omitempty"`
	RunID    string          `json:"run_id,omitempty"`
	Question string          `json:"question"`
	Options  json.RawMessage `json:"options_json,omitempty"`
	Status   string          `json:"status"`
	Answer   string          `json:"answer,omitempty"`
	// Channel is the requesting channel at create time, then overwritten with
	// the answering channel when the person replies (the last surface that
	// touched the question).
	Channel   string    `json:"channel,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) CreateClarifyRequest(ctx context.Context, req ClarifyRequest) (*ClarifyRequest, error) {
	req.TenantID = normalizeTenant(req.TenantID)
	if req.PersonID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if strings.TrimSpace(req.Question) == "" {
		return nil, fmt.Errorf("question is required")
	}
	if len(req.Options) == 0 {
		req.Options = json.RawMessage(`[]`)
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	if req.ID == "" {
		req.ID = "clr_" + uuid.NewString()
	}
	now := time.Now()
	req.CreatedAt = now
	req.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO clarify_requests
		   (id, tenant_id, person_id, task_id, run_id, question, options_json, status, answer, channel, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.TenantID, req.PersonID, req.TaskID, req.RunID, req.Question, string(req.Options), req.Status,
		req.Answer, req.Channel, req.CreatedAt.Unix(), req.UpdatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Store) ListClarifyRequests(ctx context.Context, tenantID, personID, status string, limit int) ([]ClarifyRequest, error) {
	if personID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	tenantID = normalizeTenant(tenantID)
	status = normalizeName(status, "pending")
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), question,
		        COALESCE(options_json, '[]'), status, COALESCE(answer, ''), COALESCE(channel, ''),
		        created_at, updated_at
		 FROM clarify_requests
		 WHERE tenant_id = ? AND person_id = ? AND status = ?
		 ORDER BY created_at ASC, id ASC LIMIT ?`,
		tenantID, personID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClarifyRequest
	for rows.Next() {
		item, err := scanClarifyRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetClarifyRequest(ctx context.Context, tenantID, clarifyID string) (*ClarifyRequest, error) {
	clarifyID = strings.TrimSpace(clarifyID)
	if clarifyID == "" {
		return nil, fmt.Errorf("clarify id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), question,
		        COALESCE(options_json, '[]'), status, COALESCE(answer, ''), COALESCE(channel, ''),
		        created_at, updated_at
		 FROM clarify_requests WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), clarifyID)
	item, err := scanClarifyRequest(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// AnswerClarifyRequest records the person's free-text reply and finalizes the
// question. Only a pending row is claimed; the answering channel overwrites the
// requesting channel (last surface to touch the question). The blocking waiter
// in gatewayClarify polls GetClarifyRequest and returns the answer as the tool
// result.
func (s *Store) AnswerClarifyRequest(ctx context.Context, tenantID, personID, clarifyID, answer, channel string) (*ClarifyRequest, error) {
	tenantID = normalizeTenant(tenantID)
	if personID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	clarifyID = strings.TrimSpace(clarifyID)
	if clarifyID == "" {
		return nil, fmt.Errorf("clarify id is required")
	}
	if strings.TrimSpace(answer) == "" {
		return nil, fmt.Errorf("answer is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE clarify_requests
		 SET status = 'answered', answer = ?, channel = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending'`,
		answer, channel, time.Now().Unix(), tenantID, personID, clarifyID)
	if err != nil {
		return nil, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		existing, err := s.GetClarifyRequest(ctx, tenantID, clarifyID)
		if err != nil {
			return nil, err
		}
		if existing == nil || existing.PersonID != personID {
			return nil, fmt.Errorf("clarify request not found: %s", clarifyID)
		}
		if existing.Status != "pending" {
			return nil, fmt.Errorf("clarify request %s is already %s", clarifyID, existing.Status)
		}
	}
	return s.GetClarifyRequest(ctx, tenantID, clarifyID)
}

// AnswerClarifyRequestWithResume records one person's answer and, when the
// question's origin run is parked in a resumable state, creates the exact
// continuation queue row in the same transaction. A live waiter needs no
// queue; it observes the answered row directly. The transaction prevents a
// crash from committing an answer without its child-run recovery edge.
func (s *Store) AnswerClarifyRequestWithResume(ctx context.Context, tenantID, personID, clarifyID, answer, channel string, q QueuedTask) (*ClarifyRequest, *QueuedTask, error) {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	clarifyID = strings.TrimSpace(clarifyID)
	answer = strings.TrimSpace(answer)
	if personID == "" || clarifyID == "" || answer == "" {
		return nil, nil, fmt.Errorf("clarification answer requires person, clarify id, and answer")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, owner, taskID, runID, runStatus, runPersonID, runTaskID, legacyClaim string
	if err := tx.QueryRowContext(ctx,
		`SELECT c.status, c.person_id, COALESCE(c.task_id, ''), COALESCE(c.run_id, ''),
		        COALESCE(r.status, ''), COALESCE(r.person_id, ''), COALESCE(r.task_id, ''),
		        COALESCE(r.resumed_by_run_id, '')
		 FROM clarify_requests c
		 LEFT JOIN task_runs r ON r.tenant_id = c.tenant_id AND r.id = c.run_id
		 WHERE c.tenant_id = ? AND c.id = ?`, tenantID, clarifyID,
	).Scan(&status, &owner, &taskID, &runID, &runStatus, &runPersonID, &runTaskID, &legacyClaim); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, fmt.Errorf("clarify request not found: %s", clarifyID)
		}
		return nil, nil, err
	}
	if owner != personID || status != "pending" {
		return nil, nil, fmt.Errorf("clarify request %s is not an answerable pending request", clarifyID)
	}
	expireUnavailable := func(reason string) (*ClarifyRequest, *QueuedTask, error) {
		result, updateErr := tx.ExecContext(ctx,
			`UPDATE clarify_requests SET status = 'expired', updated_at = ?
			 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending'`,
			time.Now().Unix(), tenantID, personID, clarifyID)
		if updateErr != nil {
			return nil, nil, updateErr
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return nil, nil, fmt.Errorf("clarify request %s changed while its origin was checked", clarifyID)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, nil, commitErr
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrClarifyOriginUnavailable, reason)
	}
	if runID != "" && (runPersonID != personID || taskID == "" || runTaskID != taskID) {
		return expireUnavailable("origin binding is invalid")
	}

	var queued *QueuedTask
	if runID != "" && runStatus != "running" {
		switch runStatus {
		case "interrupted", "waiting_user", "verification_partial", "blocked":
		default:
			return expireUnavailable("origin run is not resumable")
		}
		var claimed bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM task_runs WHERE tenant_id = ? AND parent_run_id = ?)`,
			tenantID, runID,
		).Scan(&claimed); err != nil {
			return nil, nil, err
		}
		if claimed || legacyClaim != "" {
			return expireUnavailable("origin run is already claimed")
		}

		q.TenantID, q.PersonID, q.TaskID, q.ClarifyID = tenantID, personID, taskID, clarifyID
		q.Platform = normalizeName(q.Platform, "cli")
		q.Channel = normalizeName(q.Channel, q.Platform)
		q.Content = answer
		q.Class = normalizeQueueClass(q.Class)
		if q.Priority == 0 {
			q.Priority = queuePriorityForClass(q.Class)
		}
		if q.ID == "" {
			q.ID = "queue_" + uuid.NewString()
		}
		if strings.TrimSpace(q.IdempotencyKey) == "" {
			q.IdempotencyKey = "clarify-resume:" + clarifyID
		}
		q.Status, q.CreatedAt = QueueStatusQueued, time.Now()
		rootsJSON, encodeErr := encodeExecutionRoots(q.ExecutionRoots)
		if encodeErr != nil {
			return nil, nil, fmt.Errorf("encode clarification resume roots: %w", encodeErr)
		}
		notBefore := q.NotBefore.Unix()
		if q.NotBefore.IsZero() {
			notBefore = 0
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO task_queue (id, tenant_id, person_id, channel, platform, platform_user_id, content, approval_mode, workspace_id, execution_roots_json, task_id, clarify_id, idempotency_key, class, priority, not_before, status, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(tenant_id, idempotency_key) WHERE idempotency_key != '' DO NOTHING`,
			q.ID, q.TenantID, q.PersonID, q.Channel, q.Platform, q.PlatformUserID, q.Content, q.ApprovalMode,
			q.WorkspaceID, rootsJSON, q.TaskID, q.ClarifyID, q.IdempotencyKey, q.Class, q.Priority, notBefore, q.Status, q.CreatedAt.Unix())
		if err != nil {
			return nil, nil, err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM task_queue WHERE tenant_id = ? AND idempotency_key = ?`,
			tenantID, q.IdempotencyKey,
		).Scan(&q.ID); err != nil {
			return nil, nil, err
		}
		queued = &q
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE clarify_requests SET status = 'answered', answer = ?, channel = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'pending'`,
		answer, channel, time.Now().Unix(), tenantID, personID, clarifyID)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, nil, fmt.Errorf("clarify request %s changed while it was being answered", clarifyID)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	clarify, err := s.GetClarifyRequest(ctx, tenantID, clarifyID)
	if err != nil {
		return nil, nil, err
	}
	return clarify, queued, nil
}

// MarkClarifyNotified stamps notified_at on a pending question once an IM
// notification has actually been SENT (initial detached push or escrow re-push).
// Idempotent, mirroring MarkApprovalNotified.
func (s *Store) MarkClarifyNotified(ctx context.Context, tenantID, clarifyID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE clarify_requests SET notified_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'pending' AND notified_at IS NULL`,
		time.Now().Unix(), normalizeTenant(tenantID), clarifyID)
	return err
}

// ListPendingClarifiesForEscrow returns pending questions created at or before
// createdBefore that have not yet had an IM notification sent, across all
// tenants, oldest first. Mirrors ListPendingApprovalsForEscrow (Fix 2).
func (s *Store) ListPendingClarifiesForEscrow(ctx context.Context, createdBefore time.Time) ([]ClarifyRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(task_id, ''), COALESCE(run_id, ''), question,
		        COALESCE(options_json, '[]'), status, COALESCE(answer, ''), COALESCE(channel, ''),
		        created_at, updated_at
		 FROM clarify_requests
		 WHERE status = 'pending' AND notified_at IS NULL AND created_at <= ?
		 ORDER BY created_at ASC, id ASC LIMIT 100`,
		createdBefore.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClarifyRequest
	for rows.Next() {
		item, err := scanClarifyRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ExpireClarifyRequest finalizes a pending question whose waiter is gone
// (timeout / run cancelled). Mirrors ExpireApprovalRequest: a stale 'pending'
// row would keep intercepting every later free-text message as an "answer" to
// a question nobody is waiting on.
func (s *Store) ExpireClarifyRequest(ctx context.Context, tenantID, clarifyID, reason string) error {
	_ = reason // schema keeps no reason column; parameter documents intent at call sites
	_, err := s.db.ExecContext(ctx,
		`UPDATE clarify_requests SET status = 'expired', updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND status = 'pending'`,
		time.Now().Unix(), tenantID, clarifyID)
	return err
}

// ExpireOrphanedClarifies expires pending questions whose origin run can no
// longer accept a continuation. Running and resumable runs keep their question:
// a daemon restart intentionally parks the run as interrupted, and the later
// answer must still be able to claim that exact parent. Questions with no run id
// are left alone.
func (s *Store) ExpireOrphanedClarifies(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clarify_requests SET status = 'expired', updated_at = ?
		 WHERE status = 'pending' AND COALESCE(run_id, '') != ''
		   AND NOT EXISTS (
		     SELECT 1 FROM task_runs r
		     WHERE r.tenant_id = clarify_requests.tenant_id
		       AND r.id = clarify_requests.run_id
		       AND (
		         r.status = 'running'
		         OR (
		           r.status IN `+resumableRunStatusSQL+`
		           AND COALESCE(r.resumed_by_run_id, '') = ''
		           AND NOT EXISTS (
		             SELECT 1 FROM task_runs child
		             WHERE child.tenant_id = r.tenant_id AND child.parent_run_id = r.id
		           )
		         )
		       )
		   )`,
		time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanClarifyRequest(rows interface {
	Scan(dest ...interface{}) error
}) (ClarifyRequest, error) {
	var item ClarifyRequest
	var options string
	var created, updated int64
	if err := rows.Scan(&item.ID, &item.TenantID, &item.PersonID, &item.TaskID, &item.RunID, &item.Question,
		&options, &item.Status, &item.Answer, &item.Channel, &created, &updated); err != nil {
		return ClarifyRequest{}, err
	}
	item.Options = json.RawMessage(options)
	item.CreatedAt = time.Unix(created, 0)
	item.UpdatedAt = time.Unix(updated, 0)
	return item, nil
}
