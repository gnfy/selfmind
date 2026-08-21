package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	MemoryGovernanceOutcomeRunning   = "running"
	MemoryGovernanceOutcomeSucceeded = "succeeded"
	MemoryGovernanceOutcomePartial   = "partial"
	MemoryGovernanceOutcomeDeferred  = "deferred"
	MemoryGovernanceOutcomeFailed    = "failed"
)

// PersonPartition identifies one durable person partition without collapsing
// the future tenant boundary into the person id.
type PersonPartition struct {
	TenantID string
	PersonID string
}

// MemoryGovernanceSchedule is the durable scheduler clock for one person's
// consolidation pass. It stores operational state only, never memory content.
type MemoryGovernanceSchedule struct {
	TenantID           string
	PersonID           string
	LastAttemptAt      time.Time
	LastSuccessAt      time.Time
	NextDueAt          time.Time
	ConsecutiveFailure int
	LastOutcome        string
	DeferredReason     string
	LastError          string
	UpdatedAt          time.Time
}

func (s *Store) ListPersonPartitions(ctx context.Context) ([]PersonPartition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, id FROM persons ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonPartition
	for rows.Next() {
		var partition PersonPartition
		if err := rows.Scan(&partition.TenantID, &partition.PersonID); err != nil {
			return nil, err
		}
		out = append(out, partition)
	}
	return out, rows.Err()
}

func (s *Store) EnsureMemoryGovernanceSchedule(ctx context.Context, tenantID, personID string, due time.Time) error {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return fmt.Errorf("person id is required")
	}
	if due.IsZero() {
		due = time.Now()
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_governance_schedule
		(tenant_id, person_id, next_due_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(tenant_id, person_id) DO NOTHING`, tenantID, personID, due.Unix(), now)
	return err
}

func (s *Store) MemoryGovernanceScheduleForPerson(ctx context.Context, tenantID, personID string) (MemoryGovernanceSchedule, bool, error) {
	var schedule MemoryGovernanceSchedule
	var lastAttempt, lastSuccess, nextDue, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id, person_id, last_attempt_at,
		last_success_at, next_due_at, consecutive_failures, last_outcome,
		last_deferred_reason, last_error, updated_at
		FROM memory_governance_schedule WHERE tenant_id = ? AND person_id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(personID)).Scan(
		&schedule.TenantID, &schedule.PersonID, &lastAttempt, &lastSuccess,
		&nextDue, &schedule.ConsecutiveFailure, &schedule.LastOutcome,
		&schedule.DeferredReason, &schedule.LastError, &updated)
	if err != nil {
		if err == sql.ErrNoRows {
			return MemoryGovernanceSchedule{}, false, nil
		}
		return MemoryGovernanceSchedule{}, false, err
	}
	schedule.LastAttemptAt = unixTimeOrZero(lastAttempt)
	schedule.LastSuccessAt = unixTimeOrZero(lastSuccess)
	schedule.NextDueAt = unixTimeOrZero(nextDue)
	schedule.UpdatedAt = unixTimeOrZero(updated)
	return schedule, true, nil
}

// RecordMemoryGovernanceAttempt marks a pass in flight AND advances next_due_at
// to retryDue, which acts as a crash lease. Without it a process killed
// mid-pass (OOM in a large cluster judge, provider panic) left the row overdue
// with last_outcome='running' and consecutive_failures untouched, so every
// restart re-ran the identical pass after the startup grace and re-burned model
// budget with no backoff and no diagnostic. Success and failure both overwrite
// next_due_at, so the lease only survives an abnormal exit.
func (s *Store) RecordMemoryGovernanceAttempt(ctx context.Context, tenantID, personID string, at, retryDue time.Time) error {
	if retryDue.Before(at) {
		retryDue = at
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memory_governance_schedule
		SET last_attempt_at = ?, next_due_at = ?, last_outcome = ?,
			last_deferred_reason = '', last_error = '', updated_at = ?
		WHERE tenant_id = ? AND person_id = ?`, at.Unix(), retryDue.Unix(),
		MemoryGovernanceOutcomeRunning, at.Unix(),
		normalizeTenant(tenantID), strings.TrimSpace(personID))
	return err
}

// ReconcileInterruptedMemoryGovernance converts rows still marked in flight into
// visible failures. The daemon is single-instance, so any 'running' row present
// at startup belongs to a process that exited before recording an outcome.
// Counting it advances consecutive_failures, which is what makes an escalating
// backoff and the diagnostic surface work at all. Returns the number of
// partitions reconciled.
func (s *Store) ReconcileInterruptedMemoryGovernance(ctx context.Context, at, nextDue time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE memory_governance_schedule
		SET next_due_at = ?, consecutive_failures = consecutive_failures + 1,
			last_outcome = ?, last_deferred_reason = '',
			last_error = 'interrupted before completion', updated_at = ?
		WHERE last_outcome = ?`, nextDue.Unix(), MemoryGovernanceOutcomeFailed,
		at.Unix(), MemoryGovernanceOutcomeRunning)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) RecordMemoryGovernanceSuccess(ctx context.Context, tenantID, personID string, at, nextDue time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memory_governance_schedule
		SET last_success_at = ?, next_due_at = ?, consecutive_failures = 0,
			last_outcome = ?, last_deferred_reason = '', last_error = '', updated_at = ?
		WHERE tenant_id = ? AND person_id = ?`, at.Unix(), nextDue.Unix(),
		MemoryGovernanceOutcomeSucceeded, at.Unix(), normalizeTenant(tenantID), strings.TrimSpace(personID))
	return err
}

// RecordMemoryGovernancePartial records healthy bounded progress without
// advancing last_success_at. A complete scan and a batch that merely made
// progress have different liveness meanings: only the former proves the
// current judge-version backlog reached zero.
func (s *Store) RecordMemoryGovernancePartial(ctx context.Context, tenantID, personID, reason string, at, nextDue time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memory_governance_schedule
		SET next_due_at = ?, consecutive_failures = 0, last_outcome = ?,
			last_deferred_reason = ?, last_error = '', updated_at = ?
		WHERE tenant_id = ? AND person_id = ?`, nextDue.Unix(),
		MemoryGovernanceOutcomePartial, strings.TrimSpace(reason), at.Unix(),
		normalizeTenant(tenantID), strings.TrimSpace(personID))
	return err
}

func (s *Store) RecordMemoryGovernanceDeferred(ctx context.Context, tenantID, personID, reason string, at, nextDue time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memory_governance_schedule
		SET next_due_at = ?, last_outcome = ?, last_deferred_reason = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ?`, nextDue.Unix(),
		MemoryGovernanceOutcomeDeferred, strings.TrimSpace(reason), at.Unix(),
		normalizeTenant(tenantID), strings.TrimSpace(personID))
	return err
}

func (s *Store) RecordMemoryGovernanceFailure(ctx context.Context, tenantID, personID, detail string, at, nextDue time.Time) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 500 {
		detail = detail[:500]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memory_governance_schedule
		SET next_due_at = ?, consecutive_failures = consecutive_failures + 1,
			last_outcome = ?, last_deferred_reason = '', last_error = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ?`, nextDue.Unix(),
		MemoryGovernanceOutcomeFailed, detail, at.Unix(), normalizeTenant(tenantID), strings.TrimSpace(personID))
	return err
}

func unixTimeOrZero(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}
