package control

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	TaskKindInteraction = "interaction"
	TaskKindWork        = "work"
	TaskKindRecurring   = "recurring"
	TaskKindInbox       = "inbox"

	TaskVisibilityVisible = "visible"
	TaskVisibilityHidden  = "hidden"
	// Thread-era values coexist with the legacy names only while callers move
	// through the WorkTimeline seam. They describe presentation, never run
	// execution authority.
	TaskVisibilityListed   = "listed"
	TaskVisibilityUnlisted = "unlisted"
	TaskVisibilityArchived = "archived"
)

func normalizeTaskKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case TaskKindInteraction:
		return TaskKindInteraction
	case TaskKindRecurring:
		return TaskKindRecurring
	case TaskKindInbox:
		return TaskKindInbox
	default:
		return TaskKindWork
	}
}

func normalizeTaskVisibility(visibility string) string {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case TaskVisibilityHidden, TaskVisibilityUnlisted:
		return TaskVisibilityUnlisted
	case TaskVisibilityArchived:
		return TaskVisibilityArchived
	case TaskVisibilityListed, TaskVisibilityVisible:
		return TaskVisibilityListed
	}
	return TaskVisibilityListed
}

func (t Task) IsVisible() bool {
	visibility := normalizeTaskVisibility(t.Visibility)
	return visibility == TaskVisibilityVisible || visibility == TaskVisibilityListed
}

func (t Task) IsInbox() bool {
	return normalizeTaskKind(t.Kind) == TaskKindInbox
}

// threadDerivedStatusSQL projects one Thread's current state, over the threads
// alias t, from the same facts Attention reads: an executing Run, a pending
// approval or clarification joined through its undismissed Run, a live
// watcher, then the latest Run that satisfies resumableRunConditionSQL. A
// Thread with none of these is settled and reports the caller's settled word
// ('done' for the compatibility Task projection, 'settled' for recall cards).
// Every status surface must use this projection so a stale or superseded Run
// cannot keep history artificially open on one surface only.
func threadDerivedStatusSQL(settled string) string {
	return `CASE
	 WHEN EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id=t.tenant_id AND r.thread_id=t.id AND r.status='running') THEN 'running'
	 WHEN EXISTS (SELECT 1 FROM approval_requests a JOIN runs r ON r.id=a.run_id AND r.thread_id=a.thread_id
	        WHERE a.tenant_id=t.tenant_id AND a.thread_id=t.id AND a.status='pending' AND COALESCE(r.attention_dismissed_at,0)=0)
	   OR EXISTS (SELECT 1 FROM clarify_requests c JOIN runs r ON r.id=c.run_id AND r.thread_id=c.thread_id
	        WHERE c.tenant_id=t.tenant_id AND c.thread_id=t.id AND c.status='pending' AND COALESCE(r.attention_dismissed_at,0)=0) THEN 'waiting_user'
	 WHEN EXISTS (SELECT 1 FROM external_watches w JOIN runs r ON r.id=w.run_id AND r.thread_id=w.thread_id
	        WHERE w.tenant_id=t.tenant_id AND w.thread_id=t.id AND w.status IN ('pending','running') AND COALESCE(r.attention_dismissed_at,0)=0) THEN 'waiting_external'
	 ELSE COALESCE((SELECT r.status FROM runs r WHERE r.tenant_id=t.tenant_id AND r.thread_id=t.id
	   AND ` + resumableRunConditionSQL + `
	   ORDER BY r.started_at DESC, r.id DESC LIMIT 1), '` + settled + `') END`
}

// Person-setting keys for the one-shot /resume selection. The Thread pin is
// the presentation fallback CurrentTask reads; the Run pin is the exact Run
// the next agent-bound message continues. They are written together by
// PinResumeSelection and consumed together by the gateway.
const (
	ResumePinThreadSettingKey = "resume_pin_task"
	ResumePinRunSettingKey    = "resume_pin_run"
)

// PinResumeSelection records an explicit /resume choice atomically: the Thread
// is reopened (an explicit selection may restore archived work) and both pins
// are written in one transaction, so a crash can never leave a Thread pin
// without its exact Run or a Run pin whose Thread stayed archived.
func (s *Store) PinResumeSelection(ctx context.Context, tenantID, personID, threadID, runID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	threadID = strings.TrimSpace(threadID)
	runID = strings.TrimSpace(runID)
	if personID == "" || threadID == "" || runID == "" {
		return fmt.Errorf("person, thread, and run ids are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE threads
		SET kind = ?, visibility = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		TaskKindWork, TaskVisibilityListed, now, tenantID, personID, threadID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("thread not found: %s", threadID)
	}
	// Choosing a Run for /resume is the person saying this work is live again,
	// so it also lifts that Run's Attention dismissal. Without this a dismissed
	// Run stayed hidden between the pin and the follow-up message even though
	// the person had just asked for it back.
	if _, err := tx.ExecContext(ctx, `UPDATE runs
		SET attention_dismissed_at = 0, attention_dismissed_by = ''
		WHERE tenant_id = ? AND person_id = ? AND id = ?`, tenantID, personID, runID); err != nil {
		return err
	}
	for key, value := range map[string]string{ResumePinThreadSettingKey: threadID, ResumePinRunSettingKey: runID} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO person_settings (tenant_id, person_id, key, value, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(tenant_id, person_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			tenantID, personID, key, value, now); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		`DELETE FROM threads
		 WHERE tenant_id = ? AND person_id = ? AND id = ?
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = threads.id)
		   AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.thread_id = threads.id)
		   AND NOT EXISTS (SELECT 1 FROM task_artifacts a WHERE a.thread_id = threads.id)
		   AND NOT EXISTS (SELECT 1 FROM task_handoffs h WHERE h.thread_id = threads.id)
		   AND NOT EXISTS (SELECT 1 FROM approval_requests p WHERE p.thread_id = threads.id)
		   AND NOT EXISTS (SELECT 1 FROM clarify_requests q WHERE q.thread_id = threads.id)
		   AND NOT EXISTS (SELECT 1 FROM task_references r WHERE r.tenant_id = threads.tenant_id AND r.thread_id = threads.id)`,
		tenantID, strings.TrimSpace(personID), strings.TrimSpace(taskID))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
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
		`UPDATE threads SET pinned = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
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
	threads, err := NewWorkTimeline(s).Search(ctx, tenantID, personID, query, limit)
	if err != nil {
		return nil, err
	}
	return s.projectThreadsAsTasks(ctx, threads)
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
	if view == "open" {
		return s.queryOpenTasks(ctx, tenantID, personID, q)
	}
	threadView := map[string]string{"done": ThreadViewSettled, "archived": ThreadViewArchived, "all": ThreadViewAll}[view]
	if threadView == "" {
		return TaskPage{}, fmt.Errorf("unsupported task view: %s", q.View)
	}
	listed, err := NewWorkTimeline(s).List(ctx, tenantID, personID, ThreadQuery{
		View: threadView, WorkspaceID: q.WorkspaceID, Limit: 200,
	})
	if err != nil {
		return TaskPage{}, err
	}
	threads, total := listed.Threads, listed.Total
	if q.Keyword != "" {
		matched, err := NewWorkTimeline(s).Search(ctx, tenantID, personID, q.Keyword, 200)
		if err != nil {
			return TaskPage{}, err
		}
		allowed := make(map[string]bool, len(matched))
		for _, thread := range matched {
			allowed[thread.ID] = true
		}
		filtered := threads[:0]
		for _, thread := range threads {
			if allowed[thread.ID] {
				filtered = append(filtered, thread)
			}
		}
		threads, total = filtered, len(filtered)
	}
	if q.Status != "" {
		projected, err := s.projectThreadsAsTasks(ctx, threads)
		if err != nil {
			return TaskPage{}, err
		}
		filtered := make([]Task, 0, len(projected))
		for _, task := range projected {
			if task.Status == q.Status {
				filtered = append(filtered, task)
			}
		}
		return paginateTaskProjection(filtered, q.Limit, q.Offset), nil
	}
	projected, err := s.projectThreadsAsTasks(ctx, threads)
	if err != nil {
		return TaskPage{}, err
	}
	page := paginateTaskProjection(projected, q.Limit, q.Offset)
	page.Total = total
	return page, nil
}

// queryOpenTasks answers the open view from the ranked Attention set. With no
// keyword, status, or workspace filter the database returns exactly the
// requested page and the person's true Attention total, so a caller can reach
// past any one page and still report an honest count. Those filters are not
// part of the ranked query, so a filtered open view ranks one bounded read and
// pages the filtered projection; its Total is the number of matches within
// that read.
func (s *Store) queryOpenTasks(ctx context.Context, tenantID, personID string, q TaskQuery) (TaskPage, error) {
	timeline := NewWorkTimeline(s)
	filtered := q.Keyword != "" || q.Status != "" || q.WorkspaceID != ""
	limit, offset := q.Limit, q.Offset
	if filtered {
		limit, offset = attentionMaxPageLimit, 0
	}
	attention, total, err := timeline.AttentionPage(ctx, tenantID, personID, "", limit, offset)
	if err != nil {
		return TaskPage{}, err
	}
	var allowed map[string]bool
	if q.Keyword != "" {
		matched, err := timeline.Search(ctx, tenantID, personID, q.Keyword, 200)
		if err != nil {
			return TaskPage{}, err
		}
		allowed = make(map[string]bool, len(matched))
		for _, thread := range matched {
			allowed[thread.ID] = true
		}
	}
	tasks := make([]Task, 0, len(attention))
	for _, item := range attention {
		if q.WorkspaceID != "" && item.Thread.WorkspaceID != q.WorkspaceID {
			continue
		}
		if allowed != nil && !allowed[item.Thread.ID] {
			continue
		}
		task := taskFromAttentionItem(item)
		if q.Status == "" || task.Status == q.Status {
			tasks = append(tasks, task)
		}
	}
	if filtered {
		return paginateTaskProjection(tasks, q.Limit, q.Offset), nil
	}
	return TaskPage{Tasks: tasks, Total: total, Limit: q.Limit, Offset: q.Offset}, nil
}

// taskFromAttentionItem projects one Attention row as the compatibility Task
// card: Status is the derived activity and ResumeRunID the exact Run.
func taskFromAttentionItem(item AttentionItem) Task {
	thread := item.Thread
	task := Task{
		ID: thread.ID, TenantID: thread.TenantID, PersonID: thread.PersonID,
		WorkspaceID: thread.WorkspaceID, Title: thread.Title, Status: item.Activity,
		Kind: thread.Kind, Visibility: thread.Visibility, Pinned: thread.Pinned,
		CurrentSummary: thread.Summary, ResumeRunID: item.RunID,
		CreatedAt: thread.CreatedAt, UpdatedAt: thread.UpdatedAt, LastActivityAt: thread.LastActivityAt,
	}
	if strings.TrimSpace(item.RunSummary) != "" {
		task.CurrentSummary = item.RunSummary
	}
	return task
}

func (s *Store) projectThreadsAsTasks(ctx context.Context, threads []Thread) ([]Task, error) {
	out := make([]Task, 0, len(threads))
	for _, thread := range threads {
		task, err := s.GetTask(ctx, thread.TenantID, thread.ID)
		if err != nil {
			return nil, err
		}
		if task != nil {
			out = append(out, *task)
		}
	}
	return out, nil
}

func paginateTaskProjection(tasks []Task, limit, offset int) TaskPage {
	page := TaskPage{Total: len(tasks), Limit: limit, Offset: offset}
	if offset >= len(tasks) {
		return page
	}
	end := offset + limit
	if end > len(tasks) {
		end = len(tasks)
	}
	page.Tasks = append(page.Tasks, tasks[offset:end]...)
	return page
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
	// Open counts the whole ranked Attention set, not one page of it: /diag
	// statistics must not stop at a page bound.
	open, err := NewWorkTimeline(s).attentionTotal(ctx, tenantID, personID)
	if err != nil {
		return stats, err
	}
	stats.Open = open
	settled, err := NewWorkTimeline(s).List(ctx, tenantID, personID, ThreadQuery{View: ThreadViewSettled, Limit: 1})
	if err != nil {
		return stats, err
	}
	archived, err := NewWorkTimeline(s).List(ctx, tenantID, personID, ThreadQuery{View: ThreadViewArchived, Limit: 1})
	if err != nil {
		return stats, err
	}
	stats.Terminal, stats.Archived = settled.Total, archived.Total
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE tenant_id=? AND person_id=? AND pinned=1`,
		normalizeTenant(tenantID), strings.TrimSpace(personID)).Scan(&stats.Pinned)
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
		`SELECT id, tenant_id, person_id, 'settled', last_activity_at
		 FROM threads
		 WHERE visibility = 'listed' AND pinned = 0
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id=threads.id AND r.status='running')
		   AND NOT EXISTS (SELECT 1 FROM approval_requests a JOIN runs r ON r.id=a.run_id
		     WHERE a.thread_id=threads.id AND a.status='pending' AND COALESCE(r.attention_dismissed_at,0)=0)
		   AND NOT EXISTS (SELECT 1 FROM clarify_requests c JOIN runs r ON r.id=c.run_id
		     WHERE c.thread_id=threads.id AND c.status='pending' AND COALESCE(r.attention_dismissed_at,0)=0)
		   AND NOT EXISTS (SELECT 1 FROM external_watches w JOIN runs r ON r.id=w.run_id
		     WHERE w.thread_id=threads.id AND w.status IN ('pending','running') AND COALESCE(r.attention_dismissed_at,0)=0)
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id=threads.id AND r.status IN `+resumableRunStatusSQL+`
		     AND COALESCE(r.resumed_by_run_id,'')='' AND COALESCE(r.attention_dismissed_at,0)=0
		     AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.parent_run_id=r.id))`)
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
		threshold := doneAfter
		if threshold <= 0 {
			threshold = cancelledAfter
		}
		if threshold > 0 && age >= threshold {
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
		threshold := doneAfter
		if threshold <= 0 {
			threshold = cancelledAfter
		}
		cutoff := now.Add(-threshold).Unix()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return archived, err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE threads SET visibility = 'archived', updated_at = ?
			 WHERE tenant_id = ? AND id = ?
			   AND last_activity_at <= ?
			   AND visibility = 'listed' AND pinned = 0
			   AND NOT EXISTS (SELECT 1 FROM runs r
			                   WHERE r.thread_id = threads.id AND r.status = 'running')
			   AND NOT EXISTS (SELECT 1 FROM approval_requests a JOIN runs r ON r.id = a.run_id
			                   WHERE a.thread_id = threads.id AND a.status = 'pending'
			                     AND COALESCE(r.attention_dismissed_at, 0) = 0)
			   AND NOT EXISTS (SELECT 1 FROM clarify_requests q JOIN runs r ON r.id = q.run_id
			                   WHERE q.thread_id = threads.id AND q.status = 'pending'
			                     AND COALESCE(r.attention_dismissed_at, 0) = 0)
			   AND NOT EXISTS (SELECT 1 FROM external_watches w JOIN runs r ON r.id = w.run_id
			                   WHERE w.thread_id = threads.id AND w.status IN ('pending', 'running')
			                     AND COALESCE(r.attention_dismissed_at, 0) = 0)
			   AND NOT EXISTS (SELECT 1 FROM runs r
			                   WHERE r.thread_id = threads.id AND r.status IN `+resumableRunStatusSQL+`
			                     AND COALESCE(r.resumed_by_run_id, '') = ''
			                     AND COALESCE(r.attention_dismissed_at, 0) = 0
			                     AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.parent_run_id = r.id))`,
			now.Unix(), c.TenantID, c.TaskID, cutoff)
		if err != nil {
			_ = tx.Rollback()
			return archived, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return archived, err
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
