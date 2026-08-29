package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	// MaintenanceJobBlockedProvider pauses work until its typed route recovery
	// policy allows a new attempt. Provider-quota routes use their durable
	// half-open circuit, network routes react to an egress change, and policy
	// routes remain parked for explicit replay.
	MaintenanceJobBlockedProvider = "blocked_provider"
	// MaintenanceJobBlockedPromptRevision pauses durable work whose pinned
	// static prompt revision is missing or corrupt. Provider recovery must not
	// requeue it, because changing model routes cannot restore its contract.
	MaintenanceJobBlockedPromptRevision = "blocked_prompt_revision"
	// maintenanceRetryLimitRouteID is a durable policy identity, not a
	// physical provider route. It prevents daemon restarts and provider-route
	// recovery sweeps from replaying the same retry-exhausted job. Operators
	// can still requeue it explicitly with maintenance replay.
	maintenanceRetryLimitRouteID = "policy:retry-limit"
	// maintenanceLegacyProbeRouteID makes the one-time migration probe durable.
	// Without it, a daemon that restarts before the probe is claimed sees the
	// same blank legacy row again and replays it on every boot.
	maintenanceLegacyProbeRouteID = "policy:legacy-restart-probe"
	// Modern fatal failures without a physical route still need a durable
	// identity; leaving them blank would incorrectly classify them as legacy on
	// the next daemon start.
	maintenanceUnclassifiedProviderRouteID = "policy:provider-unclassified"
	maintenanceNetworkRoutePrefix          = "network:"
)

// MaintenanceNetworkRouteID binds a retryable maintenance failure to the
// credential-free network route selected by the provider HTTP client. The
// prefix keeps network recovery separate from physical provider-route quota
// circuits stored in the same blocked_route_id column.
func MaintenanceNetworkRouteID(fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ""
	}
	return maintenanceNetworkRoutePrefix + fingerprint
}

func isMaintenanceNetworkRouteID(routeID string) bool {
	return strings.HasPrefix(strings.TrimSpace(routeID), maintenanceNetworkRoutePrefix)
}

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
	BlockedRouteID  string
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// MaintenanceVersionMigration reports a replay-safe analyzer generation
// cutover. Terminal history remains on its original generation.
type MaintenanceVersionMigration struct {
	Eligible        int
	Migrated        int
	AlreadyCurrent  int
	MissingEvidence int
}

// MigrateMaintenanceJobsToVersion copies unfinished jobs with durable replay
// evidence from older generations. It never rewrites old rows, replays
// succeeded/skipped history, or overwrites a current-generation row.
func (s *Store) MigrateMaintenanceJobsToVersion(ctx context.Context, targetVersion int) (MaintenanceVersionMigration, error) {
	var result MaintenanceVersionMigration
	if s == nil || s.db == nil {
		return result, fmt.Errorf("control store is unavailable")
	}
	if targetVersion <= 0 {
		return result, fmt.Errorf("target analyzer version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT run_id, analyzer_version, tenant_id, status, attempts,
		next_retry_at, result_hash, payload_json, proposal_json, blocked_route_id, last_error, created_at, updated_at
		FROM maintenance_jobs
		WHERE analyzer_version < ? AND status IN (?, ?, ?, ?, ?)
		ORDER BY created_at ASC, analyzer_version DESC`, targetVersion,
		MaintenanceJobPending, MaintenanceJobFailed, MaintenanceJobRunning, MaintenanceJobBlockedProvider,
		MaintenanceJobBlockedPromptRevision)
	if err != nil {
		return result, err
	}
	var jobs []MaintenanceJob
	for rows.Next() {
		var job MaintenanceJob
		var nextRetry, createdAt, updatedAt int64
		if err := rows.Scan(&job.RunID, &job.AnalyzerVersion, &job.TenantID, &job.Status, &job.Attempts,
			&nextRetry, &job.ResultHash, &job.PayloadJSON, &job.ProposalJSON, &job.BlockedRouteID,
			&job.LastError, &createdAt, &updatedAt); err != nil {
			_ = rows.Close()
			return result, err
		}
		job.NextRetryAt = time.Unix(nextRetry, 0)
		job.CreatedAt = time.Unix(createdAt, 0)
		job.UpdatedAt = time.Unix(updatedAt, 0)
		jobs = append(jobs, job)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	seenRuns := make(map[string]struct{})
	for _, job := range jobs {
		key := normalizeTenant(job.TenantID) + "\x00" + job.RunID
		if _, seen := seenRuns[key]; seen {
			continue
		}
		seenRuns[key] = struct{}{}
		if strings.TrimSpace(job.PayloadJSON) == "" && strings.TrimSpace(job.ProposalJSON) == "" {
			result.MissingEvidence++
			continue
		}
		result.Eligible++
		status := job.Status
		if status == MaintenanceJobRunning {
			status = MaintenanceJobPending
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO maintenance_jobs
			(run_id, analyzer_version, tenant_id, status, attempts, next_retry_at, result_hash,
			 payload_json, proposal_json, blocked_route_id, last_error, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			job.RunID, targetVersion, normalizeTenant(job.TenantID), status, job.Attempts,
			job.NextRetryAt.Unix(), job.ResultHash, job.PayloadJSON, job.ProposalJSON,
			job.BlockedRouteID, job.LastError, job.CreatedAt.Unix(), time.Now().Unix())
		if err != nil {
			return result, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			result.AlreadyCurrent++
		} else {
			result.Migrated++
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
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

// SetMaintenanceJobPayload is retained for replay migration and older callers.
// Live gateway finalization should use FinishRunWithMaintenancePayload so the
// terminal state and replay evidence cannot diverge across a crash boundary.
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
	if err == nil {
		s.recordMaintenanceAttempt(ctx, tenantID, runID, analyzerVersion, "skipped", reason, "")
	}
	return err
}

// MaintenanceAttempt is one append-only row of maintenance failure history.
// The job row's last_error is overwritten on every transition (the final skip
// erases the real provider error); this table is the durable timeline that
// makes "why did learning stop" answerable after the fact.
type MaintenanceAttempt struct {
	RunID           string
	AnalyzerVersion int
	Attempt         int
	Outcome         string
	Error           string
	RouteID         string
	CreatedAt       time.Time
}

// recordMaintenanceAttempt appends one history row. Best-effort by design: a
// history write must never fail the state transition it documents.
func (s *Store) recordMaintenanceAttempt(ctx context.Context, tenantID, runID string, analyzerVersion int, outcome, errText, routeID string) {
	if len(errText) > 500 {
		errText = errText[:500]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO maintenance_attempts
		(run_id, analyzer_version, tenant_id, attempt, outcome, error, route_id, created_at)
		VALUES (?, ?, ?,
			COALESCE((SELECT attempts FROM maintenance_jobs WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?), 0),
			?, ?, ?, ?)`,
		runID, analyzerVersion, normalizeTenant(tenantID),
		normalizeTenant(tenantID), runID, analyzerVersion,
		outcome, errText, routeID, time.Now().Unix())
	if err != nil {
		// History is diagnostic, not load-bearing; surface it in logs only.
		_ = err
	}
}

// RecentMaintenanceAttempts returns the newest failure-history rows for a
// tenant since the given time, newest first.
func (s *Store) RecentMaintenanceAttempts(ctx context.Context, tenantID string, since time.Time, limit int) ([]MaintenanceAttempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, analyzer_version, attempt, outcome, error, route_id, created_at
		FROM maintenance_attempts WHERE tenant_id = ? AND created_at >= ?
		ORDER BY created_at DESC, id DESC LIMIT ?`,
		normalizeTenant(tenantID), since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintenanceAttempt
	for rows.Next() {
		var a MaintenanceAttempt
		var created int64
		if err := rows.Scan(&a.RunID, &a.AnalyzerVersion, &a.Attempt, &a.Outcome, &a.Error, &a.RouteID, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// PruneMaintenanceAttempts enforces the bounded retention window of the
// append-only history (call at worker start; the table is diagnostic, not an
// audit ledger).
func (s *Store) PruneMaintenanceAttempts(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		olderThan = 30 * 24 * time.Hour
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM maintenance_attempts WHERE created_at < ?`,
		time.Now().Add(-olderThan).Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListRunnableMaintenanceJobs returns a bounded snapshot. ClaimMaintenanceJob
// remains the ownership CAS, so duplicate workers or an immediate finalizer
// racing this scan are harmless.
func (s *Store) ListRunnableMaintenanceJobs(ctx context.Context, analyzerVersion, limit int) ([]MaintenanceJob, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, analyzer_version, tenant_id, status, attempts,
		next_retry_at, result_hash, payload_json, proposal_json, blocked_route_id, last_error, created_at, updated_at
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
			&nextRetry, &job.ResultHash, &job.PayloadJSON, &job.ProposalJSON, &job.BlockedRouteID, &job.LastError, &createdAt, &updatedAt); err != nil {
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
// transaction. INSERT OR IGNORE keeps duplicate finalize paths harmless; an
// atomic finalizer may fill an older empty pending row, but never overwrites a
// frozen replay payload.
func createMaintenanceJobTx(ctx context.Context, tx *sql.Tx, tenantID, runID string, analyzerVersion int, payload string) error {
	if runID == "" {
		return nil
	}
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	now := time.Now().Unix()
	tenant := normalizeTenant(tenantID)
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_jobs
		   (run_id, analyzer_version, tenant_id, status, payload_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, analyzerVersion, tenant, MaintenanceJobPending, payload, now, now); err != nil {
		return err
	}
	if payload == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE maintenance_jobs SET payload_json = ?, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?
		   AND status IN (?, ?) AND TRIM(COALESCE(payload_json, '')) = ''`,
		payload, now, tenant, runID, analyzerVersion,
		MaintenanceJobPending, MaintenanceJobFailed)
	return err
}

// ClaimMaintenanceJob CAS-transitions pending/failed(retry-due) -> running.
// Zero rows affected means another worker already owns or finished the job —
// the caller must skip the maintenance pass, not retry it.
func (s *Store) ClaimMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int) (bool, error) {
	claimed, _, err := s.ClaimMaintenanceJobWithLimit(ctx, tenantID, runID, analyzerVersion, 0)
	return claimed, err
}

// ClaimMaintenanceJobWithLimit is the shared claim policy for immediate and
// background maintenance paths. maxAttempts <= 0 preserves the unbounded CAS
// used to materialize an already-frozen proposal. When the retry budget is
// exhausted the job becomes visibly blocked instead of being claimed again.
func (s *Store) ClaimMaintenanceJobWithLimit(ctx context.Context, tenantID, runID string, analyzerVersion, maxAttempts int) (claimed, exhausted bool, err error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	now := time.Now().Unix()
	query :=
		`UPDATE maintenance_jobs SET status = ?, attempts = attempts + 1, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?
		   AND (status = ? OR (status = ? AND next_retry_at <= ?))`
	args := []interface{}{
		MaintenanceJobRunning, now,
		normalizeTenant(tenantID), runID, analyzerVersion,
		MaintenanceJobPending, MaintenanceJobFailed, now,
	}
	if maxAttempts > 0 {
		query += ` AND attempts < ?`
		args = append(args, maxAttempts)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if n > 0 || maxAttempts <= 0 {
		return n > 0, false, nil
	}
	var status string
	var attempts int
	var lastError, blockedRouteID string
	err = s.db.QueryRowContext(ctx,
		`SELECT status, attempts, COALESCE(last_error, ''), COALESCE(blocked_route_id, '') FROM maintenance_jobs
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?`,
		normalizeTenant(tenantID), runID, analyzerVersion).Scan(&status, &attempts, &lastError, &blockedRouteID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	if attempts < maxAttempts || (status != MaintenanceJobPending && status != MaintenanceJobFailed) {
		return false, false, nil
	}
	if !isMaintenanceNetworkRouteID(blockedRouteID) {
		blockedRouteID = maintenanceRetryLimitRouteID
	}
	blocked, err := s.blockMaintenanceJobAfterRetriesForRoute(ctx, tenantID, runID, analyzerVersion, blockedRouteID, lastError)
	if err != nil {
		return false, false, err
	}
	return false, blocked || status == MaintenanceJobBlockedProvider, nil
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
	return s.FailMaintenanceJobForRoute(ctx, tenantID, runID, analyzerVersion, "", lastError, retryAfter)
}

// FailMaintenanceJobForRoute records a retryable failure together with the
// network route that observed it. A route change can then release the work
// immediately instead of waiting for the retry horizon or operator replay.
func (s *Store) FailMaintenanceJobForRoute(ctx context.Context, tenantID, runID string, analyzerVersion int, routeID, lastError string, retryAfter time.Duration) error {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, blocked_route_id = ?, last_error = ?, next_retry_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status = ?`,
		MaintenanceJobFailed, routeID, lastError, now.Add(retryAfter).Unix(), now.Unix(),
		normalizeTenant(tenantID), runID, analyzerVersion, MaintenanceJobRunning)
	if err == nil {
		s.recordMaintenanceAttempt(ctx, tenantID, runID, analyzerVersion, "failed", lastError, routeID)
	}
	return err
}

// BlockMaintenanceJob records a non-retryable provider failure. The CAS return
// lets the gateway emit one user-visible diagnostic even when the immediate
// finalizer races the durable maintenance worker.
func (s *Store) BlockMaintenanceJob(ctx context.Context, tenantID, runID string, analyzerVersion int, lastError string) (bool, error) {
	return s.BlockMaintenanceJobForRoute(ctx, tenantID, runID, analyzerVersion, "", lastError)
}

// BlockMaintenanceJobForRoute records which physical provider route caused a
// quota pause. Route-specific jobs are released only after that route passes a
// half-open probe; legacy/other fatal failures keep an empty route id.
func (s *Store) BlockMaintenanceJobForRoute(ctx context.Context, tenantID, runID string, analyzerVersion int, routeID, lastError string) (bool, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	if routeID == "" {
		routeID = maintenanceUnclassifiedProviderRouteID
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, blocked_route_id = ?, last_error = ?, next_retry_at = 0, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status = ?`,
		MaintenanceJobBlockedProvider, routeID, lastError, time.Now().Unix(),
		normalizeTenant(tenantID), runID, analyzerVersion, MaintenanceJobRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		s.recordMaintenanceAttempt(ctx, tenantID, runID, analyzerVersion, "blocked_provider", lastError, routeID)
	}
	return n > 0, err
}

// BlockMaintenanceJobForPromptRevision records a non-retryable local asset
// failure without misclassifying it as a provider outage.
func (s *Store) BlockMaintenanceJobForPromptRevision(ctx context.Context, tenantID, runID string, analyzerVersion int, lastError string) (bool, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, blocked_route_id = '', last_error = ?, next_retry_at = 0, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ? AND status = ?`,
		MaintenanceJobBlockedPromptRevision, lastError, time.Now().Unix(),
		normalizeTenant(tenantID), runID, analyzerVersion, MaintenanceJobRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		s.recordMaintenanceAttempt(ctx, tenantID, runID, analyzerVersion, "blocked_prompt_revision", lastError, "")
	}
	return n > 0, err
}

// BlockMaintenanceJobAfterRetries parks a retryable job after its bounded
// retry budget is exhausted. Unlike SkipMaintenanceJob, this state remains
// visible in maintenance health and is eligible for the existing restart
// probe after the operator repairs or changes the provider. The last concrete
// provider error is preserved instead of being replaced by a generic limit.
func (s *Store) BlockMaintenanceJobAfterRetries(ctx context.Context, tenantID, runID string, analyzerVersion int, lastError string) (bool, error) {
	return s.blockMaintenanceJobAfterRetriesForRoute(ctx, tenantID, runID, analyzerVersion, maintenanceRetryLimitRouteID, lastError)
}

func (s *Store) blockMaintenanceJobAfterRetriesForRoute(ctx context.Context, tenantID, runID string, analyzerVersion int, routeID, lastError string) (bool, error) {
	if analyzerVersion <= 0 {
		analyzerVersion = 1
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	if lastError == "" {
		lastError = "maintenance retries exhausted without a recorded provider error"
	}
	if !isMaintenanceNetworkRouteID(routeID) {
		routeID = maintenanceRetryLimitRouteID
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs
		 SET status = ?, blocked_route_id = ?,
		     last_error = CASE WHEN TRIM(COALESCE(last_error, '')) = '' THEN ? ELSE last_error END,
		     next_retry_at = 0, updated_at = ?
		 WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?
		   AND status IN (?, ?)`,
		MaintenanceJobBlockedProvider, routeID, lastError, time.Now().Unix(),
		normalizeTenant(tenantID), runID, analyzerVersion,
		MaintenanceJobPending, MaintenanceJobFailed)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		outcome := "blocked_retry_limit"
		if isMaintenanceNetworkRouteID(routeID) {
			outcome = "blocked_network"
		}
		s.recordMaintenanceAttempt(ctx, tenantID, runID, analyzerVersion, outcome, lastError, routeID)
	}
	return n > 0, err
}

// RequeueNetworkBlockedMaintenanceJobs releases retry-delayed and exhausted
// jobs only when provider egress now has a different route fingerprint. The
// comparison also captures a local proxy listener changing between reachable
// and unreachable without weakening the no-fail-open proxy policy.
func (s *Store) RequeueNetworkBlockedMaintenanceJobs(ctx context.Context, currentFingerprint string, now time.Time) (int, error) {
	currentRouteID := MaintenanceNetworkRouteID(currentFingerprint)
	if s == nil || s.db == nil || currentRouteID == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs
		 SET status = ?, attempts = 0, next_retry_at = 0, blocked_route_id = '', last_error = '', updated_at = ?
		 WHERE status IN (?, ?)
		   AND blocked_route_id LIKE ?
		   AND blocked_route_id != ?`,
		MaintenanceJobPending, now.Unix(), MaintenanceJobFailed, MaintenanceJobBlockedProvider,
		maintenanceNetworkRoutePrefix+"%", currentRouteID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ResetNetworkBlockedMaintenanceJobs grants a fresh attempt after an explicit
// daemon restart. A new process has not selected a provider target yet, so it
// cannot compare live and persisted fingerprints; the restart itself is the
// operator's recovery boundary (including env refresh --restart).
func (s *Store) ResetNetworkBlockedMaintenanceJobs(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs
		 SET status = ?, attempts = 0, next_retry_at = 0, blocked_route_id = '', last_error = '', updated_at = ?
		 WHERE status IN (?, ?) AND blocked_route_id LIKE ?`,
		MaintenanceJobPending, now, MaintenanceJobFailed, MaintenanceJobBlockedProvider,
		maintenanceNetworkRoutePrefix+"%")
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ResetLegacyBlockedMaintenanceJobs grants one migration probe only to rows
// created before route-aware quota circuits. Newly retry-exhausted jobs carry
// a durable policy route, so they are not replayed on every daemon restart.
func (s *Store) ResetLegacyBlockedMaintenanceJobs(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, blocked_route_id = ?, next_retry_at = 0, updated_at = ?
		 WHERE status = ? AND TRIM(COALESCE(blocked_route_id, '')) = ''`,
		MaintenanceJobPending, maintenanceLegacyProbeRouteID, now, MaintenanceJobBlockedProvider)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ResetBlockedMaintenanceJobs gives non-retryable provider failures one fresh
// probe after a daemon restart. If the provider is still unavailable the first
// attempt blocks again immediately; if configuration changed, the durable
// payload is replayed without losing the maintenance result.
func (s *Store) ResetBlockedMaintenanceJobs(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs SET status = ?, blocked_route_id = '', next_retry_at = 0, updated_at = ?
		 WHERE status = ?`,
		MaintenanceJobPending, now, MaintenanceJobBlockedProvider)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReplayRetryLimitedMaintenanceJobs requeues only current-generation post-run
// jobs that exhausted the old generic retry loop. Deterministically skipped
// jobs (ineligible run, invalid/missing payload) and superseded analyzer
// generations remain terminal. This is deliberately an explicit,
// tenant-scoped operation so every daemon restart cannot replay the same bad
// history forever.
func (s *Store) ReplayRetryLimitedMaintenanceJobs(ctx context.Context, tenantID string, limit int) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs
		 SET status = ?, attempts = 0, next_retry_at = 0, last_error = '', updated_at = ?
		 WHERE rowid IN (
		   SELECT mj.rowid FROM maintenance_jobs mj
		   WHERE mj.tenant_id = ?
		     AND mj.analyzer_version = (
		       SELECT MAX(current.analyzer_version)
		       FROM maintenance_jobs current
		       JOIN task_runs current_run
		         ON current_run.tenant_id = current.tenant_id AND current_run.id = current.run_id
		       WHERE current.tenant_id = mj.tenant_id
		     )
		     AND mj.analyzer_version = (
		       SELECT MAX(latest.analyzer_version) FROM maintenance_jobs latest
		       WHERE latest.tenant_id = mj.tenant_id AND latest.run_id = mj.run_id
		     )
		     AND ((mj.status = ? AND mj.blocked_route_id = ?)
		       OR (mj.status = ? AND mj.last_error = 'maintenance retry limit reached'))
		     AND TRIM(COALESCE(mj.payload_json, '')) != ''
		   ORDER BY mj.updated_at ASC, mj.run_id ASC LIMIT ?
		 )`,
		MaintenanceJobPending, now, normalizeTenant(tenantID),
		MaintenanceJobBlockedProvider, maintenanceRetryLimitRouteID,
		MaintenanceJobSkipped, limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// CountBlockedPromptRevisionMaintenanceJobs reports how much newest-generation
// work is parked on a missing pinned prompt revision. It deliberately does not
// join task_runs: background review and Skill curation jobs carry synthetic run
// keys, so a join would silently under-report exactly the kinds this status was
// added for.
func (s *Store) CountBlockedPromptRevisionMaintenanceJobs(ctx context.Context, tenantID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM maintenance_jobs mj
		 WHERE mj.tenant_id = ?
		   AND mj.status = ?
		   AND mj.analyzer_version = (
		     SELECT MAX(latest.analyzer_version) FROM maintenance_jobs latest
		     WHERE latest.tenant_id = mj.tenant_id AND latest.run_id = mj.run_id
		   )`,
		normalizeTenant(tenantID), MaintenanceJobBlockedPromptRevision).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ReplayPromptRevisionMaintenanceJobs requeues the newest generation of work
// that was paused because its pinned prompt revision was unavailable. This is
// deliberately operator-triggered: restoring the content-addressed revision
// makes the work runnable again, while replaying before restoration simply
// returns it to the same visible blocked state without spending model tokens.
// Unlike retry-limit replay, this covers every durable maintenance kind,
// including background review and Skill curation jobs whose synthetic keys do
// not join task_runs.
func (s *Store) ReplayPromptRevisionMaintenanceJobs(ctx context.Context, tenantID string, limit int) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_jobs
		 SET status = ?, attempts = 0, next_retry_at = 0, last_error = '', updated_at = ?
		 WHERE rowid IN (
		   SELECT mj.rowid FROM maintenance_jobs mj
		   WHERE mj.tenant_id = ?
		     AND mj.status = ?
		     AND mj.analyzer_version = (
		       SELECT MAX(latest.analyzer_version) FROM maintenance_jobs latest
		       WHERE latest.tenant_id = mj.tenant_id AND latest.run_id = mj.run_id
		     )
		     AND (TRIM(COALESCE(mj.payload_json, '')) != ''
		       OR TRIM(COALESCE(mj.proposal_json, '')) != '')
		   ORDER BY mj.updated_at ASC, mj.run_id ASC LIMIT ?
		 )`,
		MaintenanceJobPending, now, normalizeTenant(tenantID),
		MaintenanceJobBlockedPromptRevision, limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// MaintenanceHealth is the person-scoped background-learning health exposed
// by /diag. It intentionally reports only aggregate state and a redacted recent
// reason; raw provider payloads remain in logs.
type MaintenanceHealth struct {
	Pending         int
	Failed          int
	Running         int
	Blocked         int
	BlockedPrompt   int
	NetworkBlocked  int
	Succeeded       int
	Skipped         int
	OldestPendingAt time.Time
	LastSuccessAt   time.Time
	LastError       string
	BlockedRoutes   []ProviderRouteHealth
}

func (s *Store) MaintenanceHealthForPerson(ctx context.Context, tenantID, personID string) (MaintenanceHealth, error) {
	tenantID = normalizeTenant(tenantID)
	var health MaintenanceHealth
	var oldest, lastSuccess int64
	personPredicate := `(r.person_id = ? OR
		(CASE WHEN json_valid(COALESCE(mj.payload_json, '')) THEN COALESCE(json_extract(mj.payload_json, '$.person_id'), '') ELSE '' END) = ? OR
		(r.id IS NULL AND mj.tenant_id = ?))`
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT
		 COALESCE(SUM(CASE WHEN mj.status = ? THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status = ? THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status = ? THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status IN (?, ?) THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status = ? THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status = ? AND mj.blocked_route_id LIKE ? THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status = ? THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN mj.status = ? THEN 1 ELSE 0 END), 0),
		 COALESCE(MIN(CASE WHEN mj.status IN (?, ?, ?, ?, ?) THEN mj.created_at ELSE NULL END), 0),
		 COALESCE(MAX(CASE WHEN mj.status = ? THEN mj.updated_at ELSE NULL END), 0)
		 FROM maintenance_jobs mj
		 LEFT JOIN task_runs r ON r.tenant_id = mj.tenant_id AND r.id = mj.run_id
		 WHERE (mj.tenant_id = ? OR (r.id IS NULL AND mj.tenant_id = ?)) AND %s
		   AND mj.analyzer_version = (
		     SELECT MAX(latest.analyzer_version) FROM maintenance_jobs latest
		     WHERE latest.tenant_id = mj.tenant_id AND latest.run_id = mj.run_id
		   )`, personPredicate),
		MaintenanceJobPending, MaintenanceJobFailed, MaintenanceJobRunning,
		MaintenanceJobBlockedProvider, MaintenanceJobBlockedPromptRevision, MaintenanceJobBlockedPromptRevision,
		MaintenanceJobBlockedProvider, maintenanceNetworkRoutePrefix+"%",
		MaintenanceJobSucceeded, MaintenanceJobSkipped,
		MaintenanceJobPending, MaintenanceJobFailed, MaintenanceJobRunning,
		MaintenanceJobBlockedProvider, MaintenanceJobBlockedPromptRevision, MaintenanceJobSucceeded,
		tenantID, personID, personID, personID, personID).
		Scan(&health.Pending, &health.Failed, &health.Running, &health.Blocked, &health.BlockedPrompt, &health.NetworkBlocked,
			&health.Succeeded, &health.Skipped, &oldest, &lastSuccess)
	if err != nil {
		return health, err
	}
	if oldest > 0 {
		health.OldestPendingAt = time.Unix(oldest, 0)
	}
	if lastSuccess > 0 {
		health.LastSuccessAt = time.Unix(lastSuccess, 0)
	}
	err = s.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COALESCE(mj.last_error, '') FROM maintenance_jobs mj
		 LEFT JOIN task_runs r ON r.tenant_id = mj.tenant_id AND r.id = mj.run_id
		 WHERE (mj.tenant_id = ? OR (r.id IS NULL AND mj.tenant_id = ?)) AND %s AND mj.status IN (?, ?, ?)
		   AND mj.analyzer_version = (
		     SELECT MAX(latest.analyzer_version) FROM maintenance_jobs latest
		     WHERE latest.tenant_id = mj.tenant_id AND latest.run_id = mj.run_id
		   )
		 ORDER BY mj.updated_at DESC LIMIT 1`, personPredicate),
		tenantID, personID, personID, personID, personID,
		MaintenanceJobFailed, MaintenanceJobBlockedProvider, MaintenanceJobBlockedPromptRevision).Scan(&health.LastError)
	if err != nil && err != sql.ErrNoRows {
		return health, err
	}
	if health.Blocked == 0 {
		return health, nil
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT pr.tenant_id, pr.route_id, pr.provider, pr.model,
		pr.state, pr.failure_class, pr.consecutive_failures, pr.opened_at, pr.next_probe_at,
		pr.probe_lease_until, pr.last_error, pr.last_request_id, pr.updated_at
		FROM maintenance_jobs mj
		LEFT JOIN task_runs r ON r.tenant_id = mj.tenant_id AND r.id = mj.run_id
		JOIN provider_route_health pr ON pr.tenant_id = mj.tenant_id AND pr.route_id = mj.blocked_route_id
		WHERE (mj.tenant_id = ? OR (r.id IS NULL AND mj.tenant_id = ?)) AND %s AND mj.status = ?
		  AND mj.analyzer_version = (
		    SELECT MAX(latest.analyzer_version) FROM maintenance_jobs latest
		    WHERE latest.tenant_id = mj.tenant_id AND latest.run_id = mj.run_id
		  )
		ORDER BY pr.updated_at DESC`, personPredicate), tenantID, personID, personID, personID, personID, MaintenanceJobBlockedProvider)
	if err != nil {
		return health, err
	}
	defer rows.Close()
	for rows.Next() {
		var route ProviderRouteHealth
		var opened, next, lease, updated int64
		if err := rows.Scan(&route.TenantID, &route.RouteID, &route.Provider, &route.Model, &route.State,
			&route.FailureClass, &route.ConsecutiveFailure, &opened, &next, &lease,
			&route.LastError, &route.LastRequestID, &updated); err != nil {
			return health, err
		}
		route.OpenedAt = unixTime(opened)
		route.NextProbeAt = unixTime(next)
		route.ProbeLeaseUntil = unixTime(lease)
		route.UpdatedAt = unixTime(updated)
		health.BlockedRoutes = append(health.BlockedRoutes, route)
	}
	return health, rows.Err()
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
		`SELECT run_id, analyzer_version, tenant_id, status, attempts, next_retry_at, result_hash, payload_json, proposal_json, blocked_route_id, last_error, created_at, updated_at
		 FROM maintenance_jobs WHERE tenant_id = ? AND run_id = ? AND analyzer_version = ?`,
		normalizeTenant(tenantID), runID, analyzerVersion)
	var job MaintenanceJob
	var nextRetry, createdAt, updatedAt int64
	if err := row.Scan(&job.RunID, &job.AnalyzerVersion, &job.TenantID, &job.Status, &job.Attempts,
		&nextRetry, &job.ResultHash, &job.PayloadJSON, &job.ProposalJSON, &job.BlockedRouteID, &job.LastError, &createdAt, &updatedAt); err != nil {
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
