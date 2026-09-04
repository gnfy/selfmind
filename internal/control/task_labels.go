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
		`SELECT e.thread_id, e.payload_json
		 FROM task_events e
		 LEFT JOIN runs r ON r.id = e.run_id
		 LEFT JOIN threads t ON t.id = e.thread_id
		 WHERE COALESCE(r.tenant_id, t.tenant_id) = ? AND COALESCE(r.person_id, t.person_id) = ?
		   AND e.type IN ('run.finished', 'run.interrupted')
		   AND e.rowid = (
		     SELECT e2.rowid FROM task_events e2
		     WHERE e2.thread_id = e.thread_id AND e2.type IN ('run.finished', 'run.interrupted')
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
		`UPDATE threads SET title = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
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
		`SELECT thread_id, COUNT(*) FROM runs
		 WHERE tenant_id = ? AND person_id = ?
		 GROUP BY thread_id`,
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
		`SELECT r.thread_id, COALESCE(r.input_summary, '')
		 FROM runs r
		 WHERE r.tenant_id = ? AND r.person_id = ?
		   AND r.id = (SELECT r2.id FROM runs r2 WHERE r2.thread_id = r.thread_id
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
		`SELECT h.thread_id, COALESCE(h.changed_files_json, '[]')
		 FROM task_handoffs h
		 JOIN threads t ON t.id = h.thread_id
		 WHERE t.tenant_id = ? AND t.person_id = ?
		   AND h.id = (SELECT h2.id FROM task_handoffs h2 WHERE h2.thread_id = h.thread_id
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
			`SELECT thread_id, COUNT(*) FROM `+table+`
			 WHERE tenant_id = ? AND person_id = ? AND status = 'pending'
			   AND COALESCE(thread_id, '') != ''
			 GROUP BY thread_id`,
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
		`SELECT id, thread_id, tenant_id, person_id, COALESCE(workspace_id, ''), channel,
		        COALESCE(input_summary, ''), COALESCE(resumes_run_id, ''), status, started_at, finished_at
		 FROM runs
		 WHERE tenant_id = ? AND thread_id = ?
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
			&r.InputSummary, &r.ResumesRunID, &r.Status, &started, &finished); err != nil {
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

// SetRunWorkKey records a deterministic display hint on a run. It never
// selects a task, claims an unfinished run, or becomes a context, workspace,
// permission, or execution boundary.
func (s *Store) SetRunWorkKey(ctx context.Context, tenantID, runID, workKey string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workKey) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET work_key = ? WHERE tenant_id = ? AND id = ?`,
		strings.ToUpper(strings.TrimSpace(workKey)), normalizeTenant(tenantID), runID)
	return err
}
