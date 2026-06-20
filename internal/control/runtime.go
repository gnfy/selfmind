package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Delivery struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	PersonID       string     `json:"person_id"`
	Platform       string     `json:"platform"`
	PlatformUserID string     `json:"platform_user_id,omitempty"`
	Channel        string     `json:"channel"`
	TaskID         string     `json:"task_id,omitempty"`
	RunID          string     `json:"run_id,omitempty"`
	Content        string     `json:"content"`
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

func (s *Store) MarkInterruptedRuns(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	if olderThan <= 0 {
		cutoff = time.Now().Unix()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, tenant_id FROM task_runs
		 WHERE status = 'running' AND COALESCE(heartbeat_at, started_at) <= ?`,
		cutoff)
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
	for _, r := range runs {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE task_runs SET status = 'interrupted', finished_at = ?, last_error = 'gateway restarted before run finished'
			 WHERE tenant_id = ? AND id = ? AND status = 'running'`,
			now, r.tenantID, r.runID); err != nil {
			return 0, err
		}
		_, _ = s.db.ExecContext(ctx,
			`UPDATE tasks SET status = 'interrupted', active_run_id = '', current_summary = COALESCE(NULLIF(current_summary, ''), 'Interrupted by gateway restart.'), updated_at = ?
			 WHERE tenant_id = ? AND id = ? AND active_run_id = ?`,
			now, r.tenantID, r.taskID, r.runID)
		_, _ = s.AppendEvent(ctx, Event{
			TaskID:     r.taskID,
			RunID:      r.runID,
			Type:       "run.interrupted",
			Visibility: "task",
			Payload:    json.RawMessage(`{"reason":"gateway restarted before run finished"}`),
		})
	}
	return len(runs), nil
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
		   (id, tenant_id, person_id, platform, platform_user_id, channel, task_id, run_id, content, status, attempts, max_attempts,
		    next_attempt_at, last_error, part_index, part_total, idempotency_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
		d.ID, d.TenantID, d.PersonID, d.Platform, d.PlatformUserID, d.Channel, d.TaskID, d.RunID, d.Content, d.Status, d.Attempts,
		d.MaxAttempts, d.NextAttemptAt.Unix(), d.LastError, d.PartIndex, d.PartTotal, d.IdempotencyKey, d.CreatedAt.Unix(), d.UpdatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) ListDueDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, platform, COALESCE(platform_user_id, ''), channel, COALESCE(task_id, ''), COALESCE(run_id, ''),
		        content, status, attempts, max_attempts, next_attempt_at, COALESCE(last_error, ''),
		        part_index, part_total, COALESCE(idempotency_key, ''), created_at, updated_at, COALESCE(delivered_at, 0)
		 FROM outbound_messages
		 WHERE status IN ('pending', 'retry') AND next_attempt_at <= ?
		 ORDER BY next_attempt_at ASC, created_at ASC LIMIT ?`,
		time.Now().Unix(), limit)
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
		&d.Content, &d.Status, &d.Attempts, &d.MaxAttempts, &next, &d.LastError, &d.PartIndex, &d.PartTotal,
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
