package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrTaskHasLiveWork prevents display-level closure from racing work that
// can still produce side effects. The person must stop the active run or
// cancel the external watcher first.
var ErrTaskHasLiveWork = errors.New("task has live work")

type TaskClosureResult struct {
	Changed               bool
	ExpiredApprovals      int
	ExpiredClarifications int
	CancelledQueueRows    int
}

// CompleteTaskByUser closes a work label without rewriting historical run
// outcomes. Unlike the ordinary reducer, this is explicit person authority: an
// old unclaimed interrupted run must not keep a task open after the person says
// the work is complete. Pending questions are expired and queued continuations
// are cancelled so they cannot resurrect the closed label. Running work and
// external watchers fail closed.
func (s *Store) CompleteTaskByUser(ctx context.Context, tenantID, personID, taskID string) (TaskClosureResult, error) {
	return s.closeTaskByUser(ctx, tenantID, personID, taskID, "done")
}

// ArchiveTaskByUser is the reversible hygiene variant of explicit closure. It
// shares completion's cleanup and live-effect guard, then hides the label from
// open work until an explicit /resume reopens it.
func (s *Store) ArchiveTaskByUser(ctx context.Context, tenantID, personID, taskID string) (TaskClosureResult, error) {
	return s.closeTaskByUser(ctx, tenantID, personID, taskID, "archived")
}

func (s *Store) closeTaskByUser(ctx context.Context, tenantID, personID, taskID, targetStatus string) (TaskClosureResult, error) {
	var result TaskClosureResult
	if s == nil || s.db == nil {
		return result, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	taskID = strings.TrimSpace(taskID)
	if personID == "" || taskID == "" {
		return result, fmt.Errorf("task closure requires person and task ids")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM tasks
		 WHERE tenant_id = ? AND person_id = ? AND id = ?
		   AND COALESCE(visibility, 'visible') != 'hidden'`,
		tenantID, personID, taskID,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("task not found")
		}
		return result, err
	}
	alreadyClosed := (targetStatus == "done" && (status == "done" || status == "completed")) || status == targetStatus
	if alreadyClosed {
		return result, tx.Commit()
	}

	var live int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM task_runs
		 WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status = 'running'
		UNION ALL
		SELECT 1 FROM external_watches
		 WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status IN ('pending', 'running')
		UNION ALL
		SELECT 1 FROM task_queue
		 WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status = 'started'
	)`, tenantID, personID, taskID, tenantID, personID, taskID, tenantID, personID, taskID).Scan(&live); err != nil {
		return result, err
	}
	if live != 0 {
		return result, ErrTaskHasLiveWork
	}

	now := time.Now().Unix()
	approvalResult, err := tx.ExecContext(ctx, `UPDATE approval_requests
		SET status = 'expired', updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status = 'pending'`,
		now, tenantID, personID, taskID)
	if err != nil {
		return result, err
	}
	clarifyResult, err := tx.ExecContext(ctx, `UPDATE clarify_requests
		SET status = 'expired', updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status = 'pending'`,
		now, tenantID, personID, taskID)
	if err != nil {
		return result, err
	}
	queueResult, err := tx.ExecContext(ctx, `UPDATE task_queue
		SET status = 'cancelled', claim_token = '', lease_until = 0
		WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND status = 'queued'`,
		tenantID, personID, taskID)
	if err != nil {
		return result, err
	}
	updateResult, err := tx.ExecContext(ctx, `UPDATE tasks
		SET status = ?, blocked_reason = '', next_steps_json = '[]',
		    active_run_id = NULL,
		    archived_at = CASE WHEN ? = 'archived' THEN ? ELSE NULL END,
		    last_activity_at = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		targetStatus, targetStatus, now, now, now, tenantID, personID, taskID)
	if err != nil {
		return result, err
	}
	if n, _ := updateResult.RowsAffected(); n != 1 {
		return result, fmt.Errorf("close task affected %d rows", n)
	}
	result.Changed = true
	result.ExpiredApprovals = rowsAffected(approvalResult)
	result.ExpiredClarifications = rowsAffected(clarifyResult)
	result.CancelledQueueRows = rowsAffected(queueResult)
	if err := tx.Commit(); err != nil {
		return TaskClosureResult{}, err
	}
	return result, nil
}

func rowsAffected(result sql.Result) int {
	if result == nil {
		return 0
	}
	n, _ := result.RowsAffected()
	return int(n)
}
