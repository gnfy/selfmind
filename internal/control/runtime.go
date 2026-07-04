package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"selfmind/internal/platform/log"
)

type Delivery struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id,omitempty"`
	Channel        string `json:"channel"`
	TaskID         string `json:"task_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Content        string `json:"content"`
	// Kind marks typed outbound messages (e.g. "approval") so platform senders
	// can attach native affordances such as inline buttons; ApprovalID carries
	// the approval the buttons act on. Both survive retry because they are
	// persisted with the row.
	Kind           string     `json:"kind,omitempty"`
	ApprovalID     string     `json:"approval_id,omitempty"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	LastError      string     `json:"last_error,omitempty"`
	PartIndex      int        `json:"part_index"`
	PartTotal      int        `json:"part_total"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
}

func (s *Store) UpdateRunHeartbeat(ctx context.Context, tenantID, runID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE task_runs SET heartbeat_at = ? WHERE tenant_id = ? AND id = ? AND status = 'running'`,
		time.Now().Unix(), normalizeTenant(tenantID), runID)
	return err
}

func (s *Store) RequestRunCancel(ctx context.Context, tenantID, runID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE task_runs SET cancel_requested = 1 WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), runID)
	return err
}

func (s *Store) RunCancelRequested(ctx context.Context, tenantID, runID string) (bool, error) {
	var requested int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(cancel_requested, 0) FROM task_runs WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), runID).Scan(&requested)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return requested != 0, err
}

// ListRunningRuns returns runs still marked 'running' for a tenant, restricted
// to the given person IDs when provided. The eval harness uses it after a case
// to verify every synchronous eval turn actually reached a terminal run status
// before the harness tears down (phantom `running` rows are what polluted real
// control.db files before eval isolation).
func (s *Store) ListRunningRuns(ctx context.Context, tenantID string, personIDs []string) ([]Run, error) {
	query := `SELECT id, task_id, tenant_id, person_id, COALESCE(workspace_id, ''), channel, COALESCE(input_summary, ''), status, started_at
	          FROM task_runs WHERE tenant_id = ? AND status = 'running'`
	args := []any{normalizeTenant(tenantID)}
	if len(personIDs) > 0 {
		query += ` AND person_id IN (` + placeholders(len(personIDs)) + `)`
		args = append(args, toAnySlice(personIDs)...)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started int64
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TenantID, &r.PersonID, &r.WorkspaceID, &r.Channel, &r.InputSummary, &r.Status, &started); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(started, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkInterruptedRuns is the stuck-run recovery sweep. It flips every run
// still marked 'running' whose heartbeat (falling back to started_at) is older
// than olderThan to 'interrupted'; olderThan <= 0 means "every running run",
// which is the daemon-boot case: the gateway.lock flock guarantees no other
// process owns this control.db, so any run left 'running' at boot is dead.
// Runs listed in exceptRunIDs are never touched — the periodic in-daemon sweep
// passes the active-run registry so a live run whose heartbeat writer stalls
// (e.g. SQLite contention) can never be killed from under the agent.
//
// After the run pass it repairs orphaned tasks: any task still 'running' with
// zero 'running' runs left is flipped to 'interrupted' too, regardless of what
// active_run_id points at. Invariant: after this sweep — and after every run
// finalization path in the gateway — no task may remain 'running' without a
// live run; task status 'running' means "a run is executing right now", never
// "parked between turns". Returns the number of runs plus orphaned tasks
// recovered.
func (s *Store) MarkInterruptedRuns(ctx context.Context, olderThan time.Duration, exceptRunIDs ...string) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	if olderThan <= 0 {
		cutoff = time.Now().Unix()
	}
	query := `SELECT id, task_id, tenant_id FROM task_runs
		 WHERE status = 'running' AND COALESCE(heartbeat_at, started_at) <= ?`
	args := []any{cutoff}
	if len(exceptRunIDs) > 0 {
		query += ` AND id NOT IN (` + placeholders(len(exceptRunIDs)) + `)`
		args = append(args, toAnySlice(exceptRunIDs)...)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type row struct {
		runID    string
		taskID   string
		tenantID string
	}
	var runs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.runID, &r.taskID, &r.tenantID); err != nil {
			return 0, err
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	// Apply all run + task status flips in a single transaction so a failure
	// can't leave some runs marked interrupted while their tasks still point at
	// a dead active_run_id.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, r := range runs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE task_runs SET status = 'interrupted', finished_at = ?, last_error = 'gateway restarted before run finished'
			 WHERE tenant_id = ? AND id = ? AND status = 'running'`,
			now, r.tenantID, r.runID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'interrupted', active_run_id = '', current_summary = COALESCE(NULLIF(current_summary, ''), 'Interrupted by gateway restart.'), updated_at = ?
			 WHERE tenant_id = ? AND id = ? AND active_run_id = ?`,
			now, r.tenantID, r.taskID, r.runID); err != nil {
			return 0, err
		}
	}
	// Orphaned-task repair: the run pass above only flips a task whose
	// active_run_id still points at the dead run. Historic finalization bugs
	// (FinishRun writing a non-terminal run status while clearing
	// active_run_id) and pre-task-sync sweeps left tasks 'running' with no
	// 'running' run at all — those never matched the guard and looked
	// "running" forever in /tasks. The tx sees the run flips above, so any
	// task still 'running' here truly has zero live runs (excluded registry
	// runs keep status 'running' and protect their tasks). 'interrupted' is
	// deliberately non-terminal: resolveContinueTask still offers these tasks
	// for `继续` / `/resume`.
	orphanRows, err := tx.QueryContext(ctx,
		`SELECT id, tenant_id FROM tasks
		 WHERE status = 'running' AND NOT EXISTS (
		   SELECT 1 FROM task_runs r WHERE r.task_id = tasks.id AND r.status = 'running'
		 )`)
	if err != nil {
		return 0, err
	}
	type orphan struct {
		taskID   string
		tenantID string
	}
	var orphans []orphan
	for orphanRows.Next() {
		var o orphan
		if err := orphanRows.Scan(&o.taskID, &o.tenantID); err != nil {
			orphanRows.Close()
			return 0, err
		}
		orphans = append(orphans, o)
	}
	if err := orphanRows.Err(); err != nil {
		orphanRows.Close()
		return 0, err
	}
	orphanRows.Close()
	for _, o := range orphans {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'interrupted', active_run_id = '', current_summary = COALESCE(NULLIF(current_summary, ''), 'Interrupted: the gateway lost this task''s run before it finished.'), updated_at = ?
			 WHERE tenant_id = ? AND id = ? AND status = 'running'`,
			now, o.tenantID, o.taskID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	// Events are best-effort observability; append them after the state is
	// durably committed and log (rather than swallow) any failure.
	for _, r := range runs {
		if _, err := s.AppendEvent(ctx, Event{
			TaskID:     r.taskID,
			RunID:      r.runID,
			Type:       "run.interrupted",
			Visibility: "task",
			Payload:    json.RawMessage(`{"reason":"gateway restarted before run finished"}`),
		}); err != nil {
			log.Warn("failed to append run.interrupted event", "task_id", r.taskID, "run_id", r.runID, "error", err)
		}
	}
	for _, o := range orphans {
		if _, err := s.AppendEvent(ctx, Event{
			TaskID:     o.taskID,
			Type:       "task.interrupted",
			Visibility: "task",
			Payload:    json.RawMessage(`{"reason":"task was running with no live run"}`),
		}); err != nil {
			log.Warn("failed to append task.interrupted event", "task_id", o.taskID, "error", err)
		}
	}
	return len(runs) + len(orphans), nil
}

func (s *Store) ListTaskEvents(ctx context.Context, taskID string, limit int) ([]Event, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, COALESCE(run_id, ''), type, visibility, COALESCE(channel, ''),
		        COALESCE(payload_json, '{}'), created_at
		 FROM task_events WHERE task_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		var created int64
		if err := rows.Scan(&e.ID, &e.TaskID, &e.RunID, &e.Type, &e.Visibility, &e.Channel, &payload, &created); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt = time.Unix(created, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) EnqueueDelivery(ctx context.Context, d Delivery) (*Delivery, error) {
	now := time.Now()
	if d.TenantID == "" {
		d.TenantID = DefaultTenantID
	}
	d.TenantID = normalizeTenant(d.TenantID)
	d.Platform = normalizeName(d.Platform, "webhook")
	d.Channel = normalizeName(d.Channel, d.Platform)
	if d.PersonID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if d.Content == "" {
		return nil, fmt.Errorf("delivery content is required")
	}
	if d.ID == "" {
		d.ID = "out_" + uuid.NewString()
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = 3
	}
	if d.PartIndex <= 0 {
		d.PartIndex = 1
	}
	if d.PartTotal <= 0 {
		d.PartTotal = 1
	}
	if d.NextAttemptAt.IsZero() {
		d.NextAttemptAt = now
	}
	d.CreatedAt = now
	d.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO outbound_messages
		   (id, tenant_id, person_id, platform, platform_user_id, channel, task_id, run_id, content, kind, approval_id, status, attempts, max_attempts,
		    next_attempt_at, last_error, part_index, part_total, idempotency_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
		d.ID, d.TenantID, d.PersonID, d.Platform, d.PlatformUserID, d.Channel, d.TaskID, d.RunID, d.Content, d.Kind, d.ApprovalID, d.Status, d.Attempts,
		d.MaxAttempts, d.NextAttemptAt.Unix(), d.LastError, d.PartIndex, d.PartTotal, d.IdempotencyKey, d.CreatedAt.Unix(), d.UpdatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// staleSendingSeconds is how long a delivery may sit in 'sending' before the
// due-poller may reclaim it. A claim normally resolves in seconds; a stale one
// means the dispatcher crashed mid-send, so the row must become retryable
// instead of being stranded forever.
const staleSendingSeconds = 120

func (s *Store) ListDueDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, platform, COALESCE(platform_user_id, ''), channel, COALESCE(task_id, ''), COALESCE(run_id, ''),
		        content, COALESCE(kind, ''), COALESCE(approval_id, ''), status, attempts, max_attempts, next_attempt_at, COALESCE(last_error, ''),
		        part_index, part_total, COALESCE(idempotency_key, ''), created_at, updated_at, COALESCE(delivered_at, 0)
		 FROM outbound_messages
		 WHERE (status IN ('pending', 'retry') AND next_attempt_at <= ?)
		    OR (status = 'sending' AND updated_at <= ?)
		 ORDER BY next_attempt_at ASC, created_at ASC LIMIT ?`,
		now, now-staleSendingSeconds, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClaimDelivery atomically transitions a due delivery to 'sending' so exactly
// one dispatcher — the EnqueueAndTry immediate attempt or the retry poller —
// owns a given attempt. Without this claim the two dispatchers race between
// send and MarkDeliveryAttempt and the recipient gets the message twice
// (observed live: duplicate approval push, attempts=2). Returns false when the
// row is already claimed or no longer due; a 'sending' row older than
// staleSendingSeconds is reclaimable (dispatcher crashed mid-send).
func (s *Store) ClaimDelivery(ctx context.Context, id string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE outbound_messages SET status = 'sending', updated_at = ?
		 WHERE id = ? AND (
		   (status IN ('pending', 'retry') AND next_attempt_at <= ?)
		   OR (status = 'sending' AND updated_at <= ?)
		 )`,
		now, id, now, now-staleSendingSeconds)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkDeliverySentUnconfirmed finalizes a delivery the platform accepted but
// may silently drop (e.g. Weixin/iLink push on a stale context_token). It is
// terminal for the retry queue — resending on the same stale session risks
// duplicates — and distinct from 'sent' so the attach digest can list
// possibly-missed notifications.
func (s *Store) MarkDeliverySentUnconfirmed(ctx context.Context, id string) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbound_messages SET status = 'sent_unconfirmed', attempts = attempts + 1, last_error = '', updated_at = ?, delivered_at = ? WHERE id = ?`,
		now.Unix(), now.Unix(), id)
	return err
}

func (s *Store) MarkDeliveryAttempt(ctx context.Context, id string, success bool, errText string, nextAttempt time.Time) error {
	now := time.Now()
	if success {
		_, err := s.db.ExecContext(ctx,
			`UPDATE outbound_messages SET status = 'sent', attempts = attempts + 1, last_error = '', updated_at = ?, delivered_at = ? WHERE id = ?`,
			now.Unix(), now.Unix(), id)
		return err
	}
	if nextAttempt.IsZero() {
		nextAttempt = now.Add(time.Minute)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbound_messages
		 SET status = CASE WHEN attempts + 1 >= max_attempts THEN 'failed' ELSE 'retry' END,
		     attempts = attempts + 1, last_error = ?, next_attempt_at = ?, updated_at = ?
		 WHERE id = ?`,
		errText, nextAttempt.Unix(), now.Unix(), id)
	return err
}

func scanDelivery(rows interface {
	Scan(dest ...interface{}) error
}) (Delivery, error) {
	var d Delivery
	var next, created, updated, delivered int64
	if err := rows.Scan(&d.ID, &d.TenantID, &d.PersonID, &d.Platform, &d.PlatformUserID, &d.Channel, &d.TaskID, &d.RunID,
		&d.Content, &d.Kind, &d.ApprovalID, &d.Status, &d.Attempts, &d.MaxAttempts, &next, &d.LastError, &d.PartIndex, &d.PartTotal,
		&d.IdempotencyKey, &created, &updated, &delivered); err != nil {
		return Delivery{}, err
	}
	d.NextAttemptAt = time.Unix(next, 0)
	d.CreatedAt = time.Unix(created, 0)
	d.UpdatedAt = time.Unix(updated, 0)
	if delivered > 0 {
		t := time.Unix(delivered, 0)
		d.DeliveredAt = &t
	}
	return d, nil
}
