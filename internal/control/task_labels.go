package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Task-label maintenance (Work Timeline P3, docs/work-timeline.md "Labels").
// Tasks are demoted to work labels: a run's task_id is a display/resume handle
// that a post-run labeler may re-point, and humans may rename or archive a
// label. None of these operations touch run execution state.

// RenameTask sets a task's title. Used by the post-run labeler (first-time
// titling of an auto-created placeholder) and the /task <id> rename command.
func (s *Store) RenameTask(ctx context.Context, tenantID, taskID, title string) error {
	title = strings.TrimSpace(title)
	if taskID == "" {
		return fmt.Errorf("task id is required")
	}
	if title == "" {
		return fmt.Errorf("title is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET title = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		title, time.Now().Unix(), normalizeTenant(tenantID), taskID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return nil
}

// RunCountsByPerson returns task_id -> run count for every task the person
// owns. One grouped query; backs the /tasks aggregated view's "run: N 次"
// column.
func (s *Store) RunCountsByPerson(ctx context.Context, tenantID, personID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, COUNT(*) FROM task_runs
		 WHERE tenant_id = ? AND person_id = ?
		 GROUP BY task_id`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var taskID string
		var n int
		if err := rows.Scan(&taskID, &n); err != nil {
			return nil, err
		}
		out[taskID] = n
	}
	return out, rows.Err()
}

// ListTaskRuns returns a task's most recent runs, newest first. Read-only and
// bounded; backs /task <id> runs.
func (s *Store) ListTaskRuns(ctx context.Context, tenantID, taskID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, tenant_id, person_id, COALESCE(workspace_id, ''), channel,
		        COALESCE(input_summary, ''), status, started_at, finished_at
		 FROM task_runs
		 WHERE tenant_id = ? AND task_id = ?
		 ORDER BY started_at DESC, id DESC LIMIT ?`,
		normalizeTenant(tenantID), taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started int64
		var finished sql.NullInt64
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TenantID, &r.PersonID, &r.WorkspaceID, &r.Channel,
			&r.InputSummary, &r.Status, &started, &finished); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(started, 0)
		if finished.Valid {
			t := time.Unix(finished.Int64, 0)
			r.FinishedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReassignRun re-points one finished run from fromTaskID to toTaskID: the
// task_runs row plus that run's task_events and task_artifacts move in ONE
// transaction, so the timeline never shows a run whose events belong to a
// different label. When cleanupFrom is true (the abandoned pre-label task was
// auto-created this turn) and the source task is left with ZERO runs, the
// source's handoffs are folded into the target and the empty placeholder row
// is deleted — including any current_task pointer, which is repointed at the
// target so the person's "current task" never dangles. This is the post-run
// labeler's MOVE mechanic (docs/work-timeline.md "Labels"): labels never gate
// context, so a re-point is display-only and safe.
func (s *Store) ReassignRun(ctx context.Context, tenantID, runID, fromTaskID, toTaskID string, cleanupFrom bool) error {
	if runID == "" || toTaskID == "" {
		return fmt.Errorf("run id and target task id are required")
	}
	if fromTaskID == toTaskID {
		return nil
	}
	tenant := normalizeTenant(tenantID)
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Guard: the target label must exist and belong to the same tenant.
	var targetPerson string
	if err := tx.QueryRowContext(ctx,
		`SELECT person_id FROM tasks WHERE tenant_id = ? AND id = ?`,
		tenant, toTaskID).Scan(&targetPerson); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("target task not found: %s", toTaskID)
		}
		return err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE task_runs SET task_id = ? WHERE tenant_id = ? AND id = ?`,
		toTaskID, tenant, runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("run not found: %s", runID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE task_events SET task_id = ? WHERE run_id = ?`,
		toTaskID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE task_artifacts SET task_id = ? WHERE run_id = ?`,
		toTaskID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET updated_at = ? WHERE tenant_id = ? AND id = ?`,
		now, tenant, toTaskID); err != nil {
		return err
	}

	if cleanupFrom && fromTaskID != "" {
		var remaining int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM task_runs WHERE tenant_id = ? AND task_id = ?`,
			tenant, fromTaskID).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			// The placeholder's finalization handoff describes the moved run's
			// work: fold it into the target before deleting the empty label.
			if _, err := tx.ExecContext(ctx,
				`UPDATE task_handoffs SET task_id = ? WHERE task_id = ?`,
				toTaskID, fromTaskID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE current_task SET task_id = ?, updated_at = ? WHERE tenant_id = ? AND task_id = ?`,
				toTaskID, now, tenant, fromTaskID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM tasks WHERE tenant_id = ? AND id = ?`,
				tenant, fromTaskID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
