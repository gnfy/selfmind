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

const (
	TaskKindWork      = "work"
	TaskKindRecurring = "recurring"
	TaskKindInbox     = "inbox"

	TaskVisibilityVisible = "visible"
	TaskVisibilityHidden  = "hidden"
)

func normalizeTaskKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case TaskKindRecurring:
		return TaskKindRecurring
	case TaskKindInbox:
		return TaskKindInbox
	default:
		return TaskKindWork
	}
}

func normalizeTaskVisibility(visibility string) string {
	if strings.EqualFold(strings.TrimSpace(visibility), TaskVisibilityHidden) {
		return TaskVisibilityHidden
	}
	return TaskVisibilityVisible
}

func (t Task) IsVisible() bool {
	return normalizeTaskVisibility(t.Visibility) == TaskVisibilityVisible
}

func (t Task) IsInbox() bool {
	return normalizeTaskKind(t.Kind) == TaskKindInbox
}

// EnsureInboxTask returns the hidden, archived inbox label for one
// person/workspace. Inbox is an operational sink for casual or diagnostic
// runs: it preserves runs/events for audit without polluting /tasks or recall.
// It never becomes current_task and cannot capture a later turn.
func (s *Store) EnsureInboxTask(ctx context.Context, tenantID, personID, workspaceID string) (*Task, error) {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	workspaceID = strings.TrimSpace(workspaceID)
	if personID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	now := time.Now().Unix()
	id := "task_" + uuid.NewString()
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO tasks
		 (id, tenant_id, person_id, workspace_id, title, status, kind, visibility,
		  pinned, last_channel, archived_at, last_activity_at, created_at, updated_at)
		 VALUES (?, ?, ?, NULLIF(?, ''), 'Inbox', 'archived', 'inbox', 'hidden',
		  0, 'system', ?, ?, ?, ?)`,
		id, tenantID, personID, workspaceID, now, now, now, now)
	if err != nil {
		return nil, err
	}
	var taskID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM tasks
		 WHERE tenant_id = ? AND person_id = ? AND kind = 'inbox'
		   AND COALESCE(workspace_id, '') = ?
		 ORDER BY created_at ASC LIMIT 1`,
		tenantID, personID, workspaceID).Scan(&taskID)
	if err != nil {
		return nil, err
	}
	return s.GetTask(ctx, tenantID, taskID)
}

// ClearCurrentTaskIf removes a stale pointer only when it still targets the
// supplied label. This is used after moving a new placeholder run to Inbox;
// it cannot erase a newer explicit /resume selection that raced the cleanup.
func (s *Store) ClearCurrentTaskIf(ctx context.Context, tenantID, personID, taskID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM current_task WHERE tenant_id = ? AND person_id = ? AND task_id = ?`,
		normalizeTenant(tenantID), personID, taskID)
	return err
}

// DeleteEmptyTask removes only a label that has never acquired durable work.
// It is used when a freshly created placeholder cannot start its first run.
// Any run, event, artifact, handoff, approval, or question makes the task
// ineligible, so history-bearing labels are never auto-deleted.
func (s *Store) DeleteEmptyTask(ctx context.Context, tenantID, personID, taskID string) (bool, error) {
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM tasks
		 WHERE tenant_id = ? AND person_id = ? AND id = ?
		   AND NOT EXISTS (SELECT 1 FROM task_runs r WHERE r.task_id = tasks.id)
		   AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = tasks.id)
		   AND NOT EXISTS (SELECT 1 FROM task_artifacts a WHERE a.task_id = tasks.id)
		   AND NOT EXISTS (SELECT 1 FROM task_handoffs h WHERE h.task_id = tasks.id)
		   AND NOT EXISTS (SELECT 1 FROM approval_requests p WHERE p.task_id = tasks.id)
		   AND NOT EXISTS (SELECT 1 FROM clarify_requests q WHERE q.task_id = tasks.id)`,
		tenantID, strings.TrimSpace(personID), strings.TrimSpace(taskID))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM current_task WHERE tenant_id = ? AND person_id = ? AND task_id = ?`,
			tenantID, strings.TrimSpace(personID), strings.TrimSpace(taskID)); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) SetTaskPinned(ctx context.Context, tenantID, taskID string, pinned bool) error {
	value := 0
	if pinned {
		value = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET pinned = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		value, time.Now().Unix(), normalizeTenant(tenantID), taskID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return nil
}

// SearchTasks searches the complete visible task history for one person. It
// deliberately uses literal substring matching instead of tokenization so CJK
// queries and file names behave predictably. The default /tasks list stays
// small while archived work remains recoverable by title, summaries, prior
// inputs, next steps, and handoff file paths.
func (s *Store) SearchTasks(ctx context.Context, tenantID, personID, query string, limit int) ([]Task, error) {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var where strings.Builder
	args := []any{normalizeTenant(tenantID), strings.TrimSpace(personID)}
	for _, term := range terms {
		where.WriteString(` AND (
			instr(lower(COALESCE(tasks.id, '') || char(10) || COALESCE(tasks.title, '') || char(10) ||
			            COALESCE(tasks.status, '') || char(10) || COALESCE(tasks.kind, '') || char(10) ||
			            COALESCE(tasks.workspace_id, '') || char(10) || COALESCE(tasks.current_summary, '') || char(10) ||
			            COALESCE(tasks.next_steps_json, '')), ?) > 0
			OR EXISTS (SELECT 1 FROM task_runs r
			           WHERE r.tenant_id = tasks.tenant_id AND r.task_id = tasks.id
			             AND instr(lower(COALESCE(r.input_summary, '')), ?) > 0)
			OR EXISTS (SELECT 1 FROM task_handoffs h
			           WHERE h.task_id = tasks.id
			             AND instr(lower(COALESCE(h.summary, '') || char(10) || COALESCE(h.changed_files_json, '')), ?) > 0)
		)`)
		args = append(args, term, term, term)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''), title, status,
		        COALESCE(kind, 'work'), COALESCE(visibility, 'visible'), COALESCE(pinned, 0),
		        COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
		        COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
		        COALESCE(last_channel, ''), archived_at,
		        COALESCE(last_activity_at, updated_at), created_at, updated_at
		 FROM tasks
		 WHERE tenant_id = ? AND person_id = ?
		   AND COALESCE(visibility, 'visible') != 'hidden'`+where.String()+`
		 ORDER BY COALESCE(pinned, 0) DESC, updated_at DESC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var task Task
		var nextSteps string
		var pinned int
		var archived sql.NullInt64
		var lastActivity, created, updated int64
		if err := rows.Scan(&task.ID, &task.TenantID, &task.PersonID, &task.WorkspaceID,
			&task.Title, &task.Status, &task.Kind, &task.Visibility, &pinned,
			&task.CurrentSummary, &nextSteps, &task.BlockedReason, &task.ActiveRunID,
			&task.LastChannel, &archived, &lastActivity, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(nextSteps), &task.NextSteps)
		task.Pinned = pinned != 0
		if archived.Valid {
			at := time.Unix(archived.Int64, 0)
			task.ArchivedAt = &at
		}
		task.LastActivityAt = time.Unix(lastActivity, 0)
		task.CreatedAt = time.Unix(created, 0)
		task.UpdatedAt = time.Unix(updated, 0)
		out = append(out, task)
	}
	return out, rows.Err()
}

// TaskQuery is the storage-level task list contract used by CLI, IM, and HTTP.
// View is open, done, archived, or all. Keyword uses literal substring
// matching (including run inputs and handoff files), which remains predictable
// for CJK text. Limit/Offset provide stable pagination over the same ordering
// used by task cards.
type TaskQuery struct {
	View        string
	Status      string
	WorkspaceID string
	Keyword     string
	Limit       int
	Offset      int
}

type TaskPage struct {
	Tasks  []Task
	Total  int
	Limit  int
	Offset int
}

func (p TaskPage) HasMore() bool {
	return p.Offset+len(p.Tasks) < p.Total
}

func (s *Store) QueryTasks(ctx context.Context, tenantID, personID string, q TaskQuery) (TaskPage, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	view := strings.ToLower(strings.TrimSpace(q.View))
	if view == "" {
		view = "open"
	}

	where := strings.Builder{}
	where.WriteString(` WHERE tasks.tenant_id = ? AND tasks.person_id = ?
		AND COALESCE(tasks.visibility, 'visible') != 'hidden'`)
	args := []any{normalizeTenant(tenantID), strings.TrimSpace(personID)}
	if status := strings.TrimSpace(q.Status); status != "" {
		where.WriteString(` AND tasks.status = ?`)
		args = append(args, status)
	} else {
		switch view {
		case "open":
			where.WriteString(` AND tasks.status NOT IN ('done', 'completed', 'cancelled', 'failed', 'archived')`)
		case "done":
			where.WriteString(` AND tasks.status IN ('done', 'completed', 'cancelled', 'failed')`)
		case "archived":
			where.WriteString(` AND tasks.status = 'archived'`)
		case "all":
		default:
			return TaskPage{}, fmt.Errorf("unsupported task view: %s", q.View)
		}
	}
	if workspaceID := strings.TrimSpace(q.WorkspaceID); workspaceID != "" {
		where.WriteString(` AND COALESCE(tasks.workspace_id, '') = ?`)
		args = append(args, workspaceID)
	}
	for _, term := range strings.Fields(strings.ToLower(strings.TrimSpace(q.Keyword))) {
		where.WriteString(` AND (
			instr(lower(COALESCE(tasks.id, '') || char(10) || COALESCE(tasks.title, '') || char(10) ||
			            COALESCE(tasks.status, '') || char(10) || COALESCE(tasks.kind, '') || char(10) ||
			            COALESCE(tasks.workspace_id, '') || char(10) || COALESCE(tasks.current_summary, '') || char(10) ||
			            COALESCE(tasks.next_steps_json, '')), ?) > 0
			OR EXISTS (SELECT 1 FROM task_runs r
			           WHERE r.tenant_id = tasks.tenant_id AND r.task_id = tasks.id
			             AND instr(lower(COALESCE(r.input_summary, '')), ?) > 0)
			OR EXISTS (SELECT 1 FROM task_handoffs h
			           WHERE h.task_id = tasks.id
			             AND instr(lower(COALESCE(h.summary, '') || char(10) || COALESCE(h.changed_files_json, '')), ?) > 0)
		)`)
		args = append(args, term, term, term)
	}

	page := TaskPage{Limit: q.Limit, Offset: q.Offset}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`+where.String(), args...).Scan(&page.Total); err != nil {
		return TaskPage{}, err
	}
	selectArgs := append(append([]any(nil), args...), q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''), title, status,
		        COALESCE(kind, 'work'), COALESCE(visibility, 'visible'), COALESCE(pinned, 0),
		        COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
		        COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
		        COALESCE(last_channel, ''), archived_at,
		        COALESCE(last_activity_at, updated_at), created_at, updated_at
		 FROM tasks`+where.String()+`
		 ORDER BY COALESCE(pinned, 0) DESC, updated_at DESC, id ASC
		 LIMIT ? OFFSET ?`, selectArgs...)
	if err != nil {
		return TaskPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var task Task
		var nextSteps string
		var pinned int
		var archived sql.NullInt64
		var lastActivity, created, updated int64
		if err := rows.Scan(&task.ID, &task.TenantID, &task.PersonID, &task.WorkspaceID,
			&task.Title, &task.Status, &task.Kind, &task.Visibility, &pinned,
			&task.CurrentSummary, &nextSteps, &task.BlockedReason, &task.ActiveRunID,
			&task.LastChannel, &archived, &lastActivity, &created, &updated); err != nil {
			return TaskPage{}, err
		}
		_ = json.Unmarshal([]byte(nextSteps), &task.NextSteps)
		task.Pinned = pinned != 0
		if archived.Valid {
			at := time.Unix(archived.Int64, 0)
			task.ArchivedAt = &at
		}
		task.LastActivityAt = time.Unix(lastActivity, 0)
		task.CreatedAt = time.Unix(created, 0)
		task.UpdatedAt = time.Unix(updated, 0)
		page.Tasks = append(page.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		return TaskPage{}, err
	}
	return page, nil
}

type TaskGovernanceStats struct {
	Open      int
	Terminal  int
	Archived  int
	Pinned    int
	InboxRuns int
}

// ReadTaskGovernanceStats returns aggregate label hygiene without exposing
// hidden Inbox contents. It is cheap enough for /diag and keeps every surface
// honest about what the background maintainer has done.
func (s *Store) ReadTaskGovernanceStats(ctx context.Context, tenantID, personID string) (TaskGovernanceStats, error) {
	var stats TaskGovernanceStats
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN COALESCE(visibility, 'visible') = 'visible'
		                     AND status NOT IN ('done', 'completed', 'cancelled', 'failed', 'archived') THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN COALESCE(visibility, 'visible') = 'visible'
		                     AND status IN ('done', 'completed', 'cancelled', 'failed') THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN COALESCE(visibility, 'visible') = 'visible' AND status = 'archived' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN COALESCE(visibility, 'visible') = 'visible' AND COALESCE(pinned, 0) = 1 THEN 1 ELSE 0 END), 0),
		   (SELECT COUNT(*) FROM task_runs r
		      JOIN tasks inbox ON inbox.id = r.task_id
		     WHERE inbox.tenant_id = ? AND inbox.person_id = ? AND COALESCE(inbox.kind, 'work') = 'inbox')
		 FROM tasks WHERE tenant_id = ? AND person_id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), normalizeTenant(tenantID), strings.TrimSpace(personID)).
		Scan(&stats.Open, &stats.Terminal, &stats.Archived, &stats.Pinned, &stats.InboxRuns)
	return stats, err
}

type ArchivedTaskRef struct {
	TenantID string
	PersonID string
	TaskID   string
	Status   string
}

// ArchiveStaleTasks shelves old terminal work without deleting history.
// Pinned/hidden/active tasks and tasks with pending human input are excluded.
// A zero duration disables that status class.
func (s *Store) ArchiveStaleTasks(ctx context.Context, now time.Time, doneAfter, cancelledAfter time.Duration) ([]ArchivedTaskRef, error) {
	if doneAfter <= 0 && cancelledAfter <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, status, COALESCE(last_activity_at, updated_at)
		 FROM tasks
		 WHERE COALESCE(visibility, 'visible') = 'visible'
		   AND COALESCE(pinned, 0) = 0
		   AND COALESCE(active_run_id, '') = ''
		   AND status IN ('done', 'completed', 'cancelled', 'failed')`)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		ArchivedTaskRef
		activity int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.TaskID, &c.TenantID, &c.PersonID, &c.Status, &c.activity); err != nil {
			_ = rows.Close()
			return nil, err
		}
		age := now.Sub(time.Unix(c.activity, 0))
		isDone := c.Status == "done" || c.Status == "completed"
		if (isDone && doneAfter > 0 && age >= doneAfter) || (!isDone && cancelledAfter > 0 && age >= cancelledAfter) {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var archived []ArchivedTaskRef
	for _, c := range candidates {
		cutoff := now.Add(-cancelledAfter).Unix()
		if c.Status == "done" || c.Status == "completed" {
			cutoff = now.Add(-doneAfter).Unix()
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return archived, err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'archived', archived_at = ?, updated_at = ?
			 WHERE tenant_id = ? AND id = ? AND status = ?
			   AND COALESCE(last_activity_at, updated_at) <= ?
			   AND COALESCE(visibility, 'visible') = 'visible'
			   AND COALESCE(pinned, 0) = 0 AND COALESCE(active_run_id, '') = ''
			   AND NOT EXISTS (SELECT 1 FROM approval_requests a
			                   WHERE a.task_id = tasks.id AND a.status = 'pending')
			   AND NOT EXISTS (SELECT 1 FROM clarify_requests q
			                   WHERE q.task_id = tasks.id AND q.status = 'pending')`,
			now.Unix(), now.Unix(), c.TenantID, c.TaskID, c.Status, cutoff)
		if err != nil {
			_ = tx.Rollback()
			return archived, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return archived, err
		}
		if n > 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM current_task WHERE tenant_id = ? AND person_id = ? AND task_id = ?`,
				c.TenantID, c.PersonID, c.TaskID); err != nil {
				_ = tx.Rollback()
				return archived, err
			}
		}
		if err := tx.Commit(); err != nil {
			return archived, err
		}
		if n > 0 {
			archived = append(archived, c.ArchivedTaskRef)
		}
	}
	return archived, nil
}
