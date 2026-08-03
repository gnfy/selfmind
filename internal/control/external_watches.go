package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"selfmind/internal/executionenv"
)

const (
	ExternalWatchPending   = "pending"
	ExternalWatchRunning   = "running"
	ExternalWatchSucceeded = "succeeded"
	ExternalWatchFailed    = "failed"
	ExternalWatchTimedOut  = "timed_out"
	ExternalWatchCancelled = "cancelled"
	// ExternalWatchBlocked means the CHECK could not observe the external state
	// (its environment or its own definition failed). It is terminal and
	// deliberately distinct from failed: "we could not look" and "the operation
	// failed" have different remedies, and collapsing them is what let a
	// blocked check be reported as a failed deployment.
	ExternalWatchBlocked = "blocked_environment"
)

// externalWatchTerminalStatuses are the statuses that own completion side
// effects. Cancelled is excluded: it is born finalized and notified.
var externalWatchTerminalStatuses = []any{
	ExternalWatchSucceeded, ExternalWatchFailed, ExternalWatchTimedOut, ExternalWatchBlocked,
}

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
	Extensions            int
	// ExecutionBinding freezes the creator run's environment, identity, trust,
	// and approved capabilities without persisting secret values. It is the
	// transport contract future runners can consume unchanged.
	ExecutionBinding executionenv.Binding
	// Environment identity, captured when the watch was registered.
	//
	// A watch outlives the run that created it and survives daemon restarts, so
	// it is the execution path MOST likely to straddle an environment change. A
	// check registered while account A was active must never silently start
	// running under account B: the operator asked to watch a specific external
	// operation with a specific identity, and quietly changing who is asking can
	// read a different cluster or project entirely. These fields are non-secret
	// (fingerprints and a snapshot id, never values) and are compared before
	// every check.
	EnvironmentSnapshotID  string
	EnvironmentGeneration  int64
	PrincipalFingerprint   string
	EnvironmentFingerprint string
	CredentialSourceHash   string
	// VerdictRevision counts terminal-verdict revisions; it keys all
	// finalization products so a replay of the SAME verdict is a no-op while
	// a revised verdict emits one fresh correction run/event/notice.
	VerdictRevision int
	// Notified is separate from Finalized: durable task/event/queue products
	// may be complete while endpoint delivery still awaits a usable channel.
	Notified   bool
	LastOutput string
	LastError  string
	// FailureClass is the typed diagnosis of the last failed check, from the
	// same classifier the foreground tool path uses. CheckSignature identifies
	// "the same failure again" (class + output hash) and ConsecutiveFailures
	// counts the streak, so a watch that cannot succeed stops after a few
	// attempts instead of retrying until its deadline.
	FailureClass        string
	CheckSignature      string
	ConsecutiveFailures int
	CreatedAt           time.Time
	UpdatedAt           time.Time
	FinishedAt          *time.Time
}

// externalWatchColumns is the single SELECT list for a full watch row.
//
// It exists because the same column list was repeated in four queries against
// one positional scanner: adding a column meant editing five places, and
// missing one produces a scan/column mismatch at runtime rather than at build
// time. Every reader now shares this list and scanExternalWatch.
const externalWatchColumns = `id, tenant_id, person_id,
	COALESCE(workspace_id, ''), task_id, COALESCE(run_id, ''), COALESCE(channel, ''),
	description, cwd, command, success_pattern, failure_pattern, status,
	interval_seconds, command_timeout_seconds, timeout_at, next_check_at, attempts,
	COALESCE(extensions, 0), COALESCE(verdict_revision, 1), COALESCE(notified, 0), last_output, last_error,
	COALESCE(failure_class, ''), COALESCE(check_signature, ''), COALESCE(consecutive_failures, 0),
	COALESCE(environment_snapshot_id, ''), COALESCE(environment_generation, 0),
	COALESCE(principal_fingerprint, ''), COALESCE(environment_fingerprint, ''),
	COALESCE(credential_source_hash, ''), COALESCE(execution_binding_json, '{}'),
	created_at, updated_at, finished_at`

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
	if watch.ExecutionBinding.Version > 0 {
		if strings.TrimSpace(watch.ExecutionBinding.ID) == "" {
			watch.ExecutionBinding.ID = "binding:" + watch.ID
		}
		watch.ExecutionBinding.TenantID = watch.TenantID
		watch.ExecutionBinding.PersonID = watch.PersonID
		watch.ExecutionBinding.WorkspaceID = watch.WorkspaceID
	}
	bindingJSON, err := json.Marshal(watch.ExecutionBinding)
	if err != nil {
		return nil, fmt.Errorf("encode external watch execution binding: %w", err)
	}
	watch.Status = ExternalWatchPending
	watch.NextCheckAt = now
	watch.CreatedAt = now
	watch.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `INSERT INTO external_watches (
		id, tenant_id, person_id, workspace_id, task_id, run_id, channel,
		description, cwd, command, success_pattern, failure_pattern, status,
		interval_seconds, command_timeout_seconds, timeout_at, next_check_at,
		attempts, last_output, last_error,
		environment_snapshot_id, environment_generation, principal_fingerprint,
		environment_fingerprint, credential_source_hash, execution_binding_json,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, ?, ?, ?, ?, ?, ?, ?)`,
		watch.ID, watch.TenantID, watch.PersonID, watch.WorkspaceID, watch.TaskID,
		watch.RunID, watch.Channel, watch.Description, watch.CWD, watch.Command,
		watch.SuccessPattern, watch.FailurePattern, watch.Status,
		watch.IntervalSeconds, watch.CommandTimeoutSeconds, watch.TimeoutAt.Unix(),
		watch.NextCheckAt.Unix(),
		watch.EnvironmentSnapshotID, watch.EnvironmentGeneration, watch.PrincipalFingerprint,
		watch.EnvironmentFingerprint, watch.CredentialSourceHash, string(bindingJSON),
		now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	return &watch, nil
}

// GetExternalWatch reads one watch by id. Callers that need the current row
// (diagnostics, a parked-watch reason, a streak check) must not have to scan a
// due-list, which by design excludes a watch that was just claimed.
func (s *Store) GetExternalWatch(ctx context.Context, tenantID, id string) (*ExternalWatch, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(id))
	watch, err := scanExternalWatch(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
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

// RecordExternalWatchCheck records a checkpoint for a check that produced usable
// observation output. It clears the failure streak: a check that works again is
// new evidence, and leaving a stale streak behind would park a healthy watch.
func (s *Store) RecordExternalWatchCheck(ctx context.Context, tenantID, id, output, lastError string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET last_output = ?, last_error = ?, failure_class = '', check_signature = '',
			consecutive_failures = 0, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ?`,
		output, lastError, time.Now().Unix(), normalizeTenant(tenantID), id, ExternalWatchRunning)
	return err
}

// RecordExternalWatchFailure records a failed check and returns the length of
// the identical-failure streak, so the caller can stop a watch that is repeating
// one diagnosis instead of observing anything.
//
// The streak is computed in the UPDATE itself: reading, comparing, and writing
// from the worker would race the boot compensation pass over the same row.
func (s *Store) RecordExternalWatchFailure(ctx context.Context, tenantID, id, output, lastError, failureClass, signature string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("control store is unavailable")
	}
	tenant := normalizeTenant(tenantID)
	if _, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET last_output = ?, last_error = ?, failure_class = ?, check_signature = ?,
			consecutive_failures = CASE
				WHEN COALESCE(check_signature, '') = ? THEN COALESCE(consecutive_failures, 0) + 1
				ELSE 1 END,
			updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ?`,
		output, lastError, failureClass, signature, signature, time.Now().Unix(),
		tenant, id, ExternalWatchRunning); err != nil {
		return 0, err
	}
	var streak int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(consecutive_failures, 0)
		FROM external_watches WHERE tenant_id = ? AND id = ?`, tenant, id).Scan(&streak); err != nil {
		return 0, err
	}
	return streak, nil
}

func (s *Store) FinishExternalWatch(ctx context.Context, tenantID, id, status, output, lastError string) (bool, error) {
	switch status {
	case ExternalWatchSucceeded, ExternalWatchFailed, ExternalWatchTimedOut,
		ExternalWatchCancelled, ExternalWatchBlocked:
	default:
		return false, fmt.Errorf("invalid external watch terminal status %q", status)
	}
	now := time.Now().Unix()
	// A cancelled watch owes no completion side effects, so it is born
	// finalized and never enters the compensation scan.
	finalized := 0
	notified := 0
	if status == ExternalWatchCancelled {
		finalized = 1
		notified = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET status = ?, last_output = ?, last_error = ?, finalized = ?, notified = ?, finished_at = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status IN (?, ?)`,
		status, output, lastError, finalized, notified, now, now, normalizeTenant(tenantID), id,
		ExternalWatchPending, ExternalWatchRunning)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// ListExternalWatchesFinishedSince returns watches that entered the given
// terminal status after since, newest first. Startup verdict recovery uses it
// to re-examine timed_out watches whose recorded output already showed a
// terminal state that the (previously broken) pattern matcher missed.
func (s *Store) ListExternalWatchesFinishedSince(ctx context.Context, status string, since time.Time, limit int) ([]ExternalWatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches
		WHERE status = ? AND finished_at IS NOT NULL AND finished_at >= ?
		ORDER BY finished_at DESC LIMIT ?`,
		status, since.Unix(), limit)
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

// ExtendExternalWatchDeadline grants a watch exactly one bounded deadline
// extension. The CAS on extensions = 0 keeps a slow external operation from
// rolling its deadline forever: after the single grant the next expiry is
// final.
func (s *Store) ExtendExternalWatchDeadline(ctx context.Context, tenantID, id string, until time.Time, output string) (bool, error) {
	now := time.Now()
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET timeout_at = ?, extensions = extensions + 1, last_output = ?, last_error = '', updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ? AND COALESCE(extensions, 0) = 0`,
		until.Unix(), output, now.Unix(), normalizeTenant(tenantID), id, ExternalWatchRunning)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// MarkExternalWatchFinalized records that a terminal watch's completion side
// effects ran. CAS on finalized = 0 so a compensation pass and the normal
// completion path cannot both claim the same finalization.
func (s *Store) MarkExternalWatchFinalized(ctx context.Context, tenantID, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET finalized = 1, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND COALESCE(finalized, 0) = 0`,
		time.Now().Unix(), normalizeTenant(tenantID), id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// MarkExternalWatchNotified records that the completion notification has a
// durable delivery path. An attached CLI is not delivery by itself because it
// may disconnect before rendering the event.
func (s *Store) MarkExternalWatchNotified(ctx context.Context, tenantID, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET notified = 1, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND COALESCE(finalized, 0) = 1 AND COALESCE(notified, 0) = 0`,
		time.Now().Unix(), normalizeTenant(tenantID), id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// ListUnfinalizedExternalWatches returns terminal watches whose completion
// side effects never ran (daemon exit between the terminal-status CAS and the
// finalize step). Boot recovery compensates them.
func (s *Store) ListUnfinalizedExternalWatches(ctx context.Context, since time.Time, limit int) ([]ExternalWatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches
		WHERE status IN (?, ?, ?, ?) AND COALESCE(finalized, 0) = 0
		AND finished_at IS NOT NULL AND finished_at >= ?
		ORDER BY finished_at DESC LIMIT ?`,
		append(append([]any{}, externalWatchTerminalStatuses...), since.Unix(), limit)...)
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

// ListUnnotifiedExternalWatches returns core-finalized watches whose endpoint
// notification has not yet been durably enqueued. Delivery retries therefore
// cannot replay task/event/finalization products.
func (s *Store) ListUnnotifiedExternalWatches(ctx context.Context, since time.Time, limit int) ([]ExternalWatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches
		WHERE status IN (?, ?, ?, ?) AND COALESCE(finalized, 0) = 1 AND COALESCE(notified, 0) = 0
		AND finished_at IS NOT NULL AND finished_at >= ?
		ORDER BY finished_at ASC LIMIT ?`,
		append(append([]any{}, externalWatchTerminalStatuses...), since.Unix(), limit)...)
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

// ReviseExternalWatchVerdict corrects a finished watch from one terminal
// status to another (timed_out -> succeeded/failed) after re-matching its
// recorded output. It is a CAS on the from-status so concurrent recovery
// passes revise a watch at most once.
func (s *Store) ReviseExternalWatchVerdict(ctx context.Context, tenantID, id, from, to string) (bool, error) {
	switch to {
	case ExternalWatchSucceeded, ExternalWatchFailed:
	default:
		return false, fmt.Errorf("invalid revised external watch status %q", to)
	}
	// A revised verdict is a new outcome: reset finalized and bump the
	// revision so its completion side effects run exactly once under fresh
	// idempotency keys (and survive a crash via the compensation scan).
	result, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET status = ?, last_error = '', finalized = 0, notified = 0,
			verdict_revision = COALESCE(verdict_revision, 1) + 1, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ?`,
		to, time.Now().Unix(), normalizeTenant(tenantID), id, from)
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
	var bindingJSON string
	var timeoutAt, nextCheckAt, createdAt, updatedAt int64
	var notified int
	var finishedAt sql.NullInt64
	err := scanner.Scan(&watch.ID, &watch.TenantID, &watch.PersonID, &watch.WorkspaceID,
		&watch.TaskID, &watch.RunID, &watch.Channel, &watch.Description, &watch.CWD,
		&watch.Command, &watch.SuccessPattern, &watch.FailurePattern, &watch.Status,
		&watch.IntervalSeconds, &watch.CommandTimeoutSeconds, &timeoutAt, &nextCheckAt,
		&watch.Attempts, &watch.Extensions, &watch.VerdictRevision, &notified, &watch.LastOutput, &watch.LastError,
		&watch.FailureClass, &watch.CheckSignature, &watch.ConsecutiveFailures,
		&watch.EnvironmentSnapshotID, &watch.EnvironmentGeneration, &watch.PrincipalFingerprint,
		&watch.EnvironmentFingerprint, &watch.CredentialSourceHash,
		&bindingJSON,
		&createdAt, &updatedAt, &finishedAt)
	if err != nil {
		return ExternalWatch{}, err
	}
	watch.TimeoutAt = time.Unix(timeoutAt, 0)
	watch.NextCheckAt = time.Unix(nextCheckAt, 0)
	watch.CreatedAt = time.Unix(createdAt, 0)
	watch.UpdatedAt = time.Unix(updatedAt, 0)
	watch.Notified = notified != 0
	if err := json.Unmarshal([]byte(bindingJSON), &watch.ExecutionBinding); err != nil {
		return ExternalWatch{}, fmt.Errorf("decode external watch execution binding: %w", err)
	}
	if finishedAt.Valid {
		value := time.Unix(finishedAt.Int64, 0)
		watch.FinishedAt = &value
	}
	return watch, nil
}
