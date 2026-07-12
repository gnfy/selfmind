package control

import (
	"context"
	"database/sql"
	"time"
)

// Maintenance jobs make the post-run maintenance pass idempotent: one run has
// exactly one logical maintenance result per analyzer version, however many
// times its terminal notification is delivered or retried. The row is created
// inside the FinishRun terminal transaction; the analyzer CLAIMS it with a CAS
// before doing model work, so a duplicate finalize, an HTTP retry, or two
// racing goroutines can never double-apply memory decisions for one run.
// Bumping the analyzer version allows re-running maintenance over historic
// runs after an algorithm change without colliding with prior results.

const (
	MaintenanceJobPending   = "pending"
	MaintenanceJobRunning   = "running"
	MaintenanceJobSucceeded = "succeeded"
	MaintenanceJobFailed    = "failed"
	MaintenanceJobSkipped   = "skipped"
)

// MaintenanceJob mirrors one maintenance_jobs row.
type MaintenanceJob struct {
	RunID           string
	AnalyzerVersion int
	TenantID        string
	Status          string
	Attempts        int
	NextRetryAt     time.Time
	ResultHash      string
	PayloadJSON     string
	ProposalJSON    string
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EnqueueMaintenanceJob creates a standalone durable job outside the
// FinishRun transaction (execution-quality W7: background skill review and
// other daemon maintenance kinds). The (key, version) primary key makes the
// enqueue idempotent — a duplicate returns false without touching the
// existing row, which is the dedup half of the durable-review contract.
func (s *Store) EnqueueMaintenanceJob(ctx context.Context, tenantID, key string, version int, payload string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_jobs
		   (run_id, analyzer_version, tenant_id, status, payload_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		key, version, normalizeTenant(tenantID), MaintenanceJobPending, payload, now, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetMaintenanceJobPayload stores the immutable replay input captured by the
// normal finalization path. It is deliberately separate from FinishRun because
// the control store does not know the gateway request or structured outcome.
func (s *Store) SetMaintenanceJobPayload(ctx context.Context, tenantID, runID string, analyzerVersion int, payload string) error {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE maintenance_jobs SET payload_json = ?, updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status IN (?, ?)`,
		payload, time.Now().Unix(), normalizeTenant(tenantID), runID, analyzerVersion,
		MaintenanceJobPending, MaintenanceJobFailed)
	return err
}

// SaveMaintenanceProposal freezes the cheap-model result before applying it.
// A crash after this point reuses proposal_json instead of asking the model a
// second time and potentially producing a different memory decision.
func (s *Store) SaveMaintenanceProposal(ctx context.Context, tenantID, runID string, analyzerVersion int, proposal, resultHash string) error {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE maintenance_jobs SET proposal_json = ?, result_hash = ?, updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status = ?`,
		proposal, resultHash, time.Now().Unix(), normalizeTenant(tenantID), runID, analyzerVersion, MaintenanceJobRunning)
	return err
}

// SkipMaintenanceJob closes a terminal run that deterministically does not
// qualify for maintenance (or has no replay payload). Skipped is terminal and
// prevents an unbounded pile of permanently pending rows.
func (s *Store) SkipMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int, reason string) error {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE maintenance_jobs SET status = ?, last_error = ?, updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status IN (?, ?, ?)`,
		MaintenanceJobSkipped, reason, time.Now().Unix(), normalizeTenant(tenantID), runID, analyzerVersion,
		MaintenanceJobPending, MaintenanceJobFailed, MaintenanceJobRunning)
	return err
}

// ListRunnableMaintenanceJobs returns a bounded snapshot. ClaimMaintenanceJob
// remains the ownership CAS, so duplicate workers or an immediate finalizer
// racing this scan are harmless.
func (s *Store) ListRunnableMaintenanceJobs(ctx context.Context, analyzerVersion, limit int) ([]MaintenanceJob, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, analyzer_version, tenant_id, status, attempts,
		next_retry_at, result_hash, payload_json, proposal_json, last_error, created_at, updated_at
		FROM maintenance_jobs WHERE analyzer_version = ?
		AND (status = ? OR (status = ? AND next_retry_at <= ?))
		ORDER BY created_at ASC LIMIT ?`, analyzerVersion, MaintenanceJobPending, MaintenanceJobFailed, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintenanceJob
	for rows.Next() {
		var job MaintenanceJob
		var nextRetry, createdAt, updatedAt int64
		if err := rows.Scan(&job.RunID, &job.AnalyzerVersion, &job.TenantID, &job.Status, &job.Attempts,
			&nextRetry, &job.ResultHash, &job.PayloadJSON, &job.ProposalJSON, &job.LastError, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		job.NextRetryAt = time.Unix(nextRetry, 0)
		job.CreatedAt = time.Unix(createdAt, 0)
		job.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, job)
	}
	return out, rows.Err()
}

// createMaintenanceJobTx inserts the pending job inside an existing terminal
// transaction. INSERT OR IGNORE keeps duplicate finalize paths harmless.
func createMaintenanceJobTx(ctx context.Context, tx *sql.Tx, tenantID, runID string, analyzerVersion int) error {
	if runID == "" {
		return nil
	}
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	now := time.Now().Unix()
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_jobs (run_id, analyzer_version, tenant_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		runID, analyzerVersion, normalizeTenant(tenantID), MaintenanceJobPending, now, now)
	return err
}

// ClaimMaintenanceJob CAS-transitions pending/failed(retry-due) -> running.
// Zero rows affected means another worker already owns or finished the job —
// the caller must skip the maintenance pass, not retry it.
func (s *Store) ClaimMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int) (bool, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, attempts = attempts + 1, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?
		   AND (status = ? OR (status = ? AND next_retry_at <= ?))`,
		MaintenanceJobRunning, now,
		normalizeTenant(tenantID), runID, analyzerVersion,
		MaintenanceJobPending, MaintenanceJobFailed, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CompleteMaintenanceJob records the terminal success with the result hash so
// later retries of the same notification can be recognized as already applied.
func (s *Store) CompleteMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int, resultHash string) error {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, result_hash = ?, last_error = '', updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status = ?`,
		MaintenanceJobSucceeded, resultHash, time.Now().Unix(),
		normalizeTenant(tenantID), runID, analyzerVersion, MaintenanceJobRunning)
	return err
}

// FailMaintenanceJob parks the job as failed with a bounded retry horizon.
// Failures never block the run result — maintenance is strictly best-effort.
func (s *Store) FailMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int, lastError string, retryAfter time.Duration) error {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, last_error = ?, next_retry_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status = ?`,
		MaintenanceJobFailed, lastError, now.Add(retryAfter).Unix(), now.Unix(),
		normalizeTenant(tenantID), runID, analyzerVersion, MaintenanceJobRunning)
	return err
}

// ResetStaleMaintenanceJobs returns crashed 'running' jobs (daemon died mid
// maintenance) to pending so the next opportunity can claim them. Called from
// the same recovery sweep that marks interrupted runs.
func (s *Store) ResetStaleMaintenanceJobs(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, updated_at = ?
		 WHERE status = ? AND updated_at <= ?`,
		MaintenanceJobPending, time.Now().Unix(), MaintenanceJobRunning, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListPersonIDs enumerates every person in the control plane. Memory
// governance iterates these because run-written memory partitions are keyed
// by person id.
func (s *Store) ListPersonIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM persons ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetMaintenanceJob reads one job row (diagnostics and tests).
func (s *Store) GetMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int) (*MaintenanceJob, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, analyzer_version, tenant_id, status, attempts, next_retry_at, result_hash, payload_json, proposal_json, last_error, created_at, updated_at
		 FROM maintenance_jobs WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?`,
		normalizeTenant(tenantID), runID, analyzerVersion)
	var job MaintenanceJob
	var nextRetry, createdAt, updatedAt int64
	if err := row.Scan(&job.RunID, &job.AnalyzerVersion, &job.TenantID, &job.Status, &job.Attempts,
		&nextRetry, &job.ResultHash, &job.PayloadJSON, &job.ProposalJSON, &job.LastError, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if nextRetry > 0 {
		job.NextRetryAt = time.Unix(nextRetry, 0)
	}
	job.CreatedAt = time.Unix(createdAt, 0)
	job.UpdatedAt = time.Unix(updatedAt, 0)
	return &job, nil
}
