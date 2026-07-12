package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// MergeTasks folds one work label into another (execution-quality W3): every
// run of src — with its events, artifacts, handoffs, approvals, and questions
// — moves to dst in ONE transaction, then src is archived (never deleted; the
// merge stays auditable and dst now owns the full history). User-explicit
// only: the duplicate suggester proposes, never merges. Callers must ensure
// neither task has a live run. Returns the number of runs moved.
func (s *Store) MergeTasks(ctx context.Context, tenantID, personID, srcID, dstID string) (int, error) {
	if srcID == "" || dstID == "" {
		return 0, fmt.Errorf("source and target task ids are required")
	}
	if srcID == dstID {
		return 0, fmt.Errorf("cannot merge a task into itself")
	}
	tenant := normalizeTenant(tenantID)
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Both labels must exist, belong to the same person, and dst must be a
	// live (non-archived) label — merging INTO an archived label would hide
	// the moved history from every open view.
	var srcPerson, dstPerson string
	var dstArchived sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT person_id FROM tasks WHERE tenant_id = ? AND id = ?`,
		tenant, srcID).Scan(&srcPerson); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("source task not found: %s", srcID)
		}
		return 0, err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT person_id, archived_at FROM tasks WHERE tenant_id = ? AND id = ?`,
		tenant, dstID).Scan(&dstPerson, &dstArchived); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("target task not found: %s", dstID)
		}
		return 0, err
	}
	if personID != "" && (srcPerson != personID || dstPerson != personID) {
		return 0, fmt.Errorf("both tasks must belong to the same person")
	}
	if srcPerson != dstPerson {
		return 0, fmt.Errorf("both tasks must belong to the same person")
	}
	if dstArchived.Valid && dstArchived.Int64 > 0 {
		return 0, fmt.Errorf("target task is archived; /resume it first or pick an open task")
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE task_runs SET task_id = ? WHERE tenant_id = ? AND task_id = ?`,
		dstID, tenant, srcID)
	if err != nil {
		return 0, err
	}
	moved64, _ := res.RowsAffected()
	moved := int(moved64)
	for _, stmt := range []string{
		`UPDATE task_events SET task_id = ? WHERE task_id = ?`,
		`UPDATE task_artifacts SET task_id = ? WHERE task_id = ?`,
		`UPDATE task_handoffs SET task_id = ? WHERE task_id = ?`,
		`UPDATE approval_requests SET task_id = ? WHERE task_id = ?`,
		`UPDATE clarify_requests SET task_id = ? WHERE task_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, dstID, srcID); err != nil {
			return 0, err
		}
	}
	// A current-task pointer at src follows the work to dst.
	if _, err := tx.ExecContext(ctx,
		`UPDATE current_task SET task_id = ?, updated_at = ? WHERE tenant_id = ? AND task_id = ?`,
		dstID, now, tenant, srcID); err != nil {
		return 0, err
	}
	// Archive src in place: title and metadata survive for audit; /tasks and
	// recall exclude it. Status becomes terminal so recovery sweeps ignore it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'archived', archived_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		now, now, tenant, srcID); err != nil {
		return 0, err
	}
	// The merged-into label just gained activity.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		now, now, tenant, dstID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}

// ListDuplicateSuggestions returns the person's recorded duplicate-pair
// suggestions as task_id -> duplicate-of task id (newest suggestion wins per
// task). Suggestions live as task.duplicate_suggested events on the newer
// task; callers filter against currently visible labels — an archived or
// merged pair simply stops rendering, no cleanup pass needed.
func (s *Store) ListDuplicateSuggestions(ctx context.Context, tenantID, personID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.task_id, COALESCE(e.payload_json, '')
		 FROM task_events e JOIN tasks t ON t.id = e.task_id
		 WHERE e.type = 'task.duplicate_suggested'
		   AND t.tenant_id = ? AND t.person_id = ?
		 ORDER BY e.created_at ASC`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var taskID, payload string
		if rows.Scan(&taskID, &payload) != nil {
			continue
		}
		var p struct {
			DuplicateOf string `json:"duplicate_of"`
		}
		if json.Unmarshal([]byte(payload), &p) == nil && p.DuplicateOf != "" {
			out[taskID] = p.DuplicateOf
		}
	}
	return out, rows.Err()
}
