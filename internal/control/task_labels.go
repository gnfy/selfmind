package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Task-label maintenance (Work Timeline P3, docs/work-timeline.md "Labels").
// Tasks are demoted to work labels: a run's task_id is a display/resume handle
// that a post-run labeler may re-point, and humans may rename or archive a
// label. None of these operations touch run execution state.

// LatestRunOutcome is the terminal reason attached to a task's newest run.
// Task status alone cannot distinguish a daemon restart from a provider error
// or an intentional wait for an external system.
type LatestRunOutcome struct {
	CompletionReason string
	Resumable        bool
}

// LatestRunOutcomesByPerson returns the newest durable run outcome for every
// task. It is one grouped query for /tasks rendering, never a per-card lookup.
func (s *Store) LatestRunOutcomesByPerson(ctx context.Context, tenantID, personID string) (map[string]LatestRunOutcome, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.task_id, e.payload_json
		 FROM task_events e
		 JOIN tasks t ON t.id = e.task_id
		 WHERE t.tenant_id = ? AND t.person_id = ?
		   AND e.type IN ('run.finished', 'run.interrupted')
		   AND e.rowid = (
		     SELECT e2.rowid FROM task_events e2
		     WHERE e2.task_id = e.task_id AND e2.type IN ('run.finished', 'run.interrupted')
		     ORDER BY e2.created_at DESC, e2.rowid DESC LIMIT 1
		   )`, normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LatestRunOutcome{}
	for rows.Next() {
		var taskID string
		var raw []byte
		if err := rows.Scan(&taskID, &raw); err != nil {
			return nil, err
		}
		var payload struct {
			Outcome struct {
				CompletionReason string `json:"completion_reason"`
				Resumable        bool   `json:"resumable"`
			} `json:"outcome"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			out[taskID] = LatestRunOutcome{
				CompletionReason: strings.TrimSpace(payload.Outcome.CompletionReason),
				Resumable:        payload.Outcome.Resumable,
			}
		}
	}
	return out, rows.Err()
}

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

// LatestRunSummaries returns task_id -> the latest run's input summary for
// every task the person owns. One grouped query (correlated subquery picks the
// newest run per task); backs the /tasks card "last:" line without a per-task
// round trip.
func (s *Store) LatestRunSummaries(ctx context.Context, tenantID, personID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.task_id, COALESCE(r.input_summary, '')
		 FROM task_runs r
		 WHERE r.tenant_id = ? AND r.person_id = ?
		   AND r.id = (SELECT r2.id FROM task_runs r2 WHERE r2.task_id = r.task_id
		               ORDER BY r2.started_at DESC, r2.rowid DESC LIMIT 1)`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var taskID, summary string
		if err := rows.Scan(&taskID, &summary); err != nil {
			return nil, err
		}
		out[taskID] = summary
	}
	return out, rows.Err()
}

// LatestHandoffFilesByPerson returns task_id -> the latest handoff's changed
// files for every task the person owns. One grouped query; backs the /tasks
// card "file:" line (the primary artifact) without a per-task LatestHandoff.
func (s *Store) LatestHandoffFilesByPerson(ctx context.Context, tenantID, personID string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT h.task_id, COALESCE(h.changed_files_json, '[]')
		 FROM task_handoffs h
		 JOIN tasks t ON t.id = h.task_id
		 WHERE t.tenant_id = ? AND t.person_id = ?
		   AND h.id = (SELECT h2.id FROM task_handoffs h2 WHERE h2.task_id = h.task_id
		               ORDER BY h2.created_at DESC, h2.rowid DESC LIMIT 1)`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var taskID, filesJSON string
		if err := rows.Scan(&taskID, &filesJSON); err != nil {
			return nil, err
		}
		var files []string
		_ = json.Unmarshal([]byte(filesJSON), &files)
		if len(files) > 0 {
			out[taskID] = files
		}
	}
	return out, rows.Err()
}

// PendingCountsByTask returns task_id -> pending approval count and task_id ->
// pending question (clarify) count for the person, in two grouped queries.
// Backs the /tasks card "waiting" status and the approvals:/questions: lines.
func (s *Store) PendingCountsByTask(ctx context.Context, tenantID, personID string) (approvals, questions map[string]int, err error) {
	tenant := normalizeTenant(tenantID)
	countByTask := func(table string) (map[string]int, error) {
		rows, err := s.db.QueryContext(ctx,
			`SELECT task_id, COUNT(*) FROM `+table+`
			 WHERE tenant_id = ? AND person_id = ? AND status = 'pending'
			   AND COALESCE(task_id, '') != ''
			 GROUP BY task_id`,
			tenant, personID)
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
	if approvals, err = countByTask("approval_requests"); err != nil {
		return nil, nil, err
	}
	if questions, err = countByTask("clarify_requests"); err != nil {
		return nil, nil, err
	}
	return approvals, questions, nil
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

// SetRunWorkKey records a deterministic issue/work identity on a run. The key
// is used only to close the matching unfinished work; it never becomes a
// context, workspace, or execution boundary.
func (s *Store) SetRunWorkKey(ctx context.Context, tenantID, runID, workKey string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workKey) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE task_runs SET work_key = ? WHERE tenant_id = ? AND id = ?`,
		strings.ToUpper(strings.TrimSpace(workKey)), normalizeTenant(tenantID), runID)
	return err
}

// MarkTaskRunsResumed records which prior resumable run a deliberate
// continuation actually owns. A work key claims only exact-key predecessors.
// Without a key, exactly one unresolved predecessor is required; ambiguity is
// preserved instead of allowing one continuation to erase unrelated work that
// happens to share the same display label.
func (s *Store) MarkTaskRunsResumed(ctx context.Context, tenantID, taskID, resumedByRunID, workKey string) (int64, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(resumedByRunID) == "" {
		return 0, nil
	}
	tenant := normalizeTenant(tenantID)
	workKey = strings.ToUpper(strings.TrimSpace(workKey))
	if workKey != "" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		candidateID, candidateCount, err := unresolvedRunsForWorkKey(ctx, tx, tenant, taskID, resumedByRunID, workKey)
		if err != nil {
			return 0, err
		}
		// One issue key may contain several independent work lines. A keyed
		// continuation may claim one predecessor only when the key identifies a
		// single unresolved run; updating every matching row silently erases work.
		if candidateCount > 1 {
			return 0, tx.Commit()
		}
		if candidateCount == 1 {
			res, updateErr := tx.ExecContext(ctx,
				`UPDATE task_runs SET resumed_by_run_id = ?
				 WHERE tenant_id = ? AND id = ? AND work_key = ?
				   AND status IN ('interrupted', 'waiting_user', 'verification_partial', 'blocked')
				   AND COALESCE(resumed_by_run_id, '') = ''`,
				resumedByRunID, tenant, candidateID, workKey)
			if updateErr != nil {
				return 0, updateErr
			}
			affected, affectedErr := res.RowsAffected()
			if affectedErr != nil {
				return 0, affectedErr
			}
			if affected == 1 {
				if err := resolveOriginRunBlockersTx(ctx, tx, tenant, taskID, candidateID, resumedByRunID); err != nil {
					return 0, err
				}
			}
			if err := tx.Commit(); err != nil {
				return 0, err
			}
			return affected, nil
		}

		// Runs created before work_key was introduced have no durable identity.
		// Claim one only when it is the sole unresolved predecessor; any second
		// candidate (keyed or legacy) preserves ambiguity and requires an explicit
		// follow-up instead of guessing.
		legacyID, unambiguous, err := soleUnresolvedRun(ctx, tx, tenant, taskID, resumedByRunID)
		if err != nil {
			return 0, err
		}
		if !unambiguous || legacyID == "" {
			return 0, tx.Commit()
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE task_runs SET resumed_by_run_id = ?
			 WHERE tenant_id = ? AND id = ? AND COALESCE(work_key, '') = ''
			   AND COALESCE(resumed_by_run_id, '') = ''`,
			resumedByRunID, tenant, legacyID)
		if err != nil {
			return 0, err
		}
		if affected, affectedErr := res.RowsAffected(); affectedErr != nil {
			return 0, affectedErr
		} else if affected == 1 {
			if err := resolveOriginRunBlockersTx(ctx, tx, tenant, taskID, legacyID, resumedByRunID); err != nil {
				return 0, err
			}
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	candidateID, unambiguous, err := soleUnresolvedRun(ctx, tx, tenant, taskID, resumedByRunID)
	if err != nil {
		return 0, err
	}
	if !unambiguous {
		return 0, tx.Commit()
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE task_runs SET resumed_by_run_id = ?
		 WHERE tenant_id = ? AND id = ? AND COALESCE(resumed_by_run_id, '') = ''`,
		resumedByRunID, tenant, candidateID)
	if err != nil {
		return 0, err
	}
	if affected, affectedErr := res.RowsAffected(); affectedErr != nil {
		return 0, affectedErr
	} else if affected == 1 {
		if err := resolveOriginRunBlockersTx(ctx, tx, tenant, taskID, candidateID, resumedByRunID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func unresolvedRunsForWorkKey(ctx context.Context, tx *sql.Tx, tenantID, taskID, excludeRunID, workKey string) (string, int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM task_runs
		 WHERE tenant_id = ? AND task_id = ? AND id <> ? AND work_key = ?
		   AND status IN ('interrupted', 'waiting_user', 'verification_partial', 'blocked')
		   AND COALESCE(resumed_by_run_id, '') = ''
		 ORDER BY started_at DESC, id DESC LIMIT 2`,
		tenantID, taskID, excludeRunID, workKey)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", 0, err
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if len(candidates) == 0 {
		return "", 0, nil
	}
	return candidates[0], len(candidates), nil
}

func soleUnresolvedRun(ctx context.Context, tx *sql.Tx, tenantID, taskID, excludeRunID string) (string, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM task_runs
		 WHERE tenant_id = ? AND task_id = ? AND id <> ?
		   AND status IN ('interrupted', 'waiting_user', 'verification_partial', 'blocked')
		   AND COALESCE(resumed_by_run_id, '') = ''
		 ORDER BY started_at DESC, id DESC LIMIT 2`,
		tenantID, taskID, excludeRunID)
	if err != nil {
		return "", false, err
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return "", false, err
		}
		candidates = append(candidates, id)
	}
	if err := rows.Close(); err != nil {
		return "", false, err
	}
	if len(candidates) != 1 {
		return "", false, nil
	}
	return candidates[0], true, nil
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
		`UPDATE loop_checkpoints SET task_id = ? WHERE run_id = ?`,
		toTaskID, runID); err != nil {
		return err
	}
	// Approvals/questions raised during this run follow it too. Post-run they
	// are always terminal (pending rows expire when the waiter exits), so this
	// is referential integrity — decided rows must not point at a placeholder
	// the cleanup below may delete — and the prerequisite for any future
	// mid-run relabel.
	if _, err := tx.ExecContext(ctx,
		`UPDATE approval_requests SET task_id = ? WHERE run_id = ?`,
		toTaskID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE clarify_requests SET task_id = ? WHERE run_id = ?`,
		toTaskID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		now, now, tenant, toTaskID); err != nil {
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
