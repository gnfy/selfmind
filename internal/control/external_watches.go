package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ExternalWatchPending   = "pending"
	ExternalWatchRunning   = "running"
	ExternalWatchSucceeded = "succeeded"
	ExternalWatchFailed    = "failed"
	ExternalWatchTimedOut  = "timed_out"
	ExternalWatchCancelled = "cancelled"
)

// ExternalWatch is a durable, deterministic check owned by the daemon. It is
// intentionally separate from an agent run: waiting for CI/CD must not keep a
// model turn, worker slot, or person active-run slot alive.
type ExternalWatch struct {
	ID                    string
	TenantID              string
	PersonID              string
	WorkspaceID           string
	TaskID                string
	RunID                 string
	Channel               string
	Description           string
	CWD                   string
	Command               string
	SuccessPattern        string
	FailurePattern        string
	Status                string
	IntervalSeconds       int
	CommandTimeoutSeconds int
	TimeoutAt             time.Time
	NextCheckAt           time.Time
	Attempts              int
	LastOutput            string
	LastError             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	FinishedAt            *time.Time
}

func (s *Store) CreateExternalWatch(ctx context.Context, watch ExternalWatch) (*ExternalWatch, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	watch.TenantID = normalizeTenant(watch.TenantID)
	watch.PersonID = strings.TrimSpace(watch.PersonID)
	watch.TaskID = strings.TrimSpace(watch.TaskID)
	watch.CWD = strings.TrimSpace(watch.CWD)
	watch.Command = strings.TrimSpace(watch.Command)
	watch.SuccessPattern = strings.TrimSpace(watch.SuccessPattern)
	if watch.PersonID == "" || watch.TaskID == "" || watch.CWD == "" || watch.Command == "" || watch.SuccessPattern == "" {
		return nil, fmt.Errorf("person, task, cwd, command, and success pattern are required")
	}
	if watch.IntervalSeconds < 5 {
		watch.IntervalSeconds = 30
	}
	if watch.CommandTimeoutSeconds < 1 {
		watch.CommandTimeoutSeconds = 30
	}
	if watch.CommandTimeoutSeconds > 120 {
		watch.CommandTimeoutSeconds = 120
	}
	now := time.Now()
	if watch.TimeoutAt.IsZero() || !watch.TimeoutAt.After(now) {
		watch.TimeoutAt = now.Add(2 * time.Hour)
	}
	watch.ID = "watch_" + uuid.NewString()
	watch.Status = ExternalWatchPending
	watch.NextCheckAt = now
	watch.CreatedAt = now
	watch.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO external_watches (
		id, tenant_id, person_id, workspace_id, task_id, run_id, channel,
		description, cwd, command, success_pattern, failure_pattern, status,
		interval_seconds, command_timeout_seconds, timeout_at, next_check_at,
		attempts, last_output, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, ?)`,
		watch.ID, watch.TenantID, watch.PersonID, watch.WorkspaceID, watch.TaskID,
		watch.RunID, watch.Channel, watch.Description, watch.CWD, watch.Command,
		watch.SuccessPattern, watch.FailurePattern, watch.Status,
		watch.IntervalSeconds, watch.CommandTimeoutSeconds, watch.TimeoutAt.Unix(),
		watch.NextCheckAt.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	return &watch, nil
}

// ListDueExternalWatches returns a bounded due snapshot. ClaimExternalWatch is
// the ownership CAS, so concurrent ticks cannot execute the same check twice.
func (s *Store) ListDueExternalWatches(ctx context.Context, limit int) ([]ExternalWatch, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, person_id,
		COALESCE(workspace_id, ''), task_id, COALESCE(run_id, ''), COALESCE(channel, ''),
		description, cwd, command, success_pattern, failure_pattern, status,
		interval_seconds, command_timeout_seconds, timeout_at, next_check_at, attempts,
		last_output, last_error, created_at, updated_at, finished_at
		FROM external_watches
		WHERE status IN (?, ?) AND next_check_at <= ?
		ORDER BY next_check_at ASC LIMIT ?`,
		ExternalWatchPending, ExternalWatchRunning, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExternalWatch
	for rows.Next() {
		watch, err := scanExternalWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, watch)
	}
	return out, rows.Err()
}

func (s *Store) ClaimExternalWatch(ctx context.Context, watch ExternalWatch) (bool, error) {
	now := time.Now()
	next := now.Add(time.Duration(watch.IntervalSeconds) * time.Second)
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET status = ?, attempts = attempts + 1, next_check_at = ?, updated_at = ?
		WHERE id = ? AND tenant_id = ? AND status IN (?, ?) AND next_check_at <= ?`,
		ExternalWatchRunning, next.Unix(), now.Unix(), watch.ID, normalizeTenant(watch.TenantID),
		ExternalWatchPending, ExternalWatchRunning, now.Unix())
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

func (s *Store) RecordExternalWatchCheck(ctx context.Context, tenantID, id, output, lastError string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET last_output = ?, last_error = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ?`,
		output, lastError, time.Now().Unix(), normalizeTenant(tenantID), id, ExternalWatchRunning)
	return err
}

func (s *Store) FinishExternalWatch(ctx context.Context, tenantID, id, status, output, lastError string) (bool, error) {
	switch status {
	case ExternalWatchSucceeded, ExternalWatchFailed, ExternalWatchTimedOut, ExternalWatchCancelled:
	default:
		return false, fmt.Errorf("invalid external watch terminal status %q", status)
	}
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET status = ?, last_output = ?, last_error = ?, finished_at = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status IN (?, ?)`,
		status, output, lastError, now, now, normalizeTenant(tenantID), id,
		ExternalWatchPending, ExternalWatchRunning)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

func (s *Store) CountExternalWatchesByStatus(ctx context.Context, tenantID, personID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM external_watches
		WHERE tenant_id = ? AND person_id = ? GROUP BY status`, normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

type externalWatchScanner interface {
	Scan(dest ...any) error
}

func scanExternalWatch(scanner externalWatchScanner) (ExternalWatch, error) {
	var watch ExternalWatch
	var timeoutAt, nextCheckAt, createdAt, updatedAt int64
	var finishedAt sql.NullInt64
	err := scanner.Scan(&watch.ID, &watch.TenantID, &watch.PersonID, &watch.WorkspaceID,
		&watch.TaskID, &watch.RunID, &watch.Channel, &watch.Description, &watch.CWD,
		&watch.Command, &watch.SuccessPattern, &watch.FailurePattern, &watch.Status,
		&watch.IntervalSeconds, &watch.CommandTimeoutSeconds, &timeoutAt, &nextCheckAt,
		&watch.Attempts, &watch.LastOutput, &watch.LastError, &createdAt, &updatedAt, &finishedAt)
	if err != nil {
		return ExternalWatch{}, err
	}
	watch.TimeoutAt = time.Unix(timeoutAt, 0)
	watch.NextCheckAt = time.Unix(nextCheckAt, 0)
	watch.CreatedAt = time.Unix(createdAt, 0)
	watch.UpdatedAt = time.Unix(updatedAt, 0)
	if finishedAt.Valid {
		value := time.Unix(finishedAt.Int64, 0)
		watch.FinishedAt = &value
	}
	return watch, nil
}
