package control

import (
	"context"
	"crypto/sha256"
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

	WatchCheckerOK                = "ok"
	WatchCheckerCapabilityBlocked = "capability_blocked"
	WatchCheckerCommandFailed     = "command_failed"
	WatchOperationPending         = "pending"
	WatchOperationRunning         = "running"
	WatchOperationSucceeded       = "succeeded"
	WatchOperationFailed          = "failed"
	WatchOperationTimedOut        = "timed_out"
	WatchVerificationNotRequired  = "not_required"
	WatchVerificationPending      = "pending"
	WatchVerificationPassed       = "passed"
	WatchVerificationBlocked      = "blocked_environment"
	WatchVerificationFailed       = "failed"
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
	ID                     string
	TenantID               string
	PersonID               string
	WorkspaceID            string
	TaskID                 string
	RunID                  string
	Channel                string
	Description            string
	CWD                    string
	Command                string
	SuccessPattern         string
	FailurePattern         string
	SpecVersion            int
	TargetPattern          string
	TerminalSuccessPattern string
	TerminalFailurePattern string
	// ObservationAdapter opts spec v3 into a typed three-state parser owned by
	// the tool registry. Empty retains the v1/v2 regex compatibility path.
	ObservationAdapter     string
	PreflightReceipt       ExternalWatchPreflightReceipt
	WaitGroupID            string
	Status                 string
	CheckerStatus          string
	OperationStatus        string
	VerificationStatus     string
	IntervalSeconds        int
	CurrentIntervalSeconds int
	CommandTimeoutSeconds  int
	TimeoutAt              time.Time
	NextCheckAt            time.Time
	Attempts               int
	Extensions             int
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
	// Finalized records whether the durable task/event/queue products for this
	// verdict have been materialized. It is separate from endpoint delivery.
	Finalized bool
	// Notified is separate from Finalized: durable task/event/queue products
	// may be complete while endpoint delivery still awaits a usable channel.
	Notified       bool
	LastOutput     string
	LastOutputHash string
	LastError      string
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

type ExternalWatchPreflightReceipt struct {
	Version               int      `json:"version"`
	CommandHash           string   `json:"command_hash"`
	EnvironmentGeneration int64    `json:"environment_generation"`
	Adapter               string   `json:"adapter"`
	Target                string   `json:"target"`
	DeadlineUnix          int64    `json:"deadline_unix"`
	Capabilities          []string `json:"capabilities,omitempty"`
}

// externalWatchColumns is the single SELECT list for a full watch row.
//
// It exists because the same column list was repeated in four queries against
// one positional scanner: adding a column meant editing five places, and
// missing one produces a scan/column mismatch at runtime rather than at build
// time. Every reader now shares this list and scanExternalWatch.
const externalWatchColumns = `id, tenant_id, person_id,
	COALESCE(workspace_id, ''), thread_id, COALESCE(run_id, ''), COALESCE(channel, ''),
	description, cwd, command, success_pattern, failure_pattern,
	COALESCE(spec_version, 1), COALESCE(target_pattern, ''),
	COALESCE(terminal_success_pattern, ''), COALESCE(terminal_failure_pattern, ''),
	COALESCE(observation_adapter, ''), COALESCE(preflight_receipt_json, '{}'), COALESCE(wait_group_id, ''), status,
	COALESCE(checker_status, ''), COALESCE(operation_status, 'pending'), COALESCE(verification_status, 'not_required'),
	interval_seconds, COALESCE(current_interval_seconds, 0), command_timeout_seconds, timeout_at, next_check_at, attempts,
	COALESCE(extensions, 0), COALESCE(verdict_revision, 1), COALESCE(finalized, 0), COALESCE(notified, 0), last_output, last_error,
	COALESCE(last_output_hash, ''),
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
	watch.FailurePattern = strings.TrimSpace(watch.FailurePattern)
	watch.TargetPattern = strings.TrimSpace(watch.TargetPattern)
	watch.TerminalSuccessPattern = strings.TrimSpace(watch.TerminalSuccessPattern)
	watch.TerminalFailurePattern = strings.TrimSpace(watch.TerminalFailurePattern)
	watch.ObservationAdapter = strings.TrimSpace(watch.ObservationAdapter)
	if watch.SpecVersion <= 0 {
		if watch.ObservationAdapter != "" {
			watch.SpecVersion = 3
		} else if watch.TargetPattern != "" || watch.TerminalSuccessPattern != "" || watch.TerminalFailurePattern != "" {
			watch.SpecVersion = 2
		} else {
			watch.SpecVersion = 1
		}
	}
	// The run is the authority for thread membership: a tool scope frozen before
	// a direct continuation claim still names the interaction placeholder.
	if threadID := s.threadIDForRun(ctx, watch.TenantID, watch.RunID); threadID != "" {
		watch.TaskID = threadID
	}
	if watch.PersonID == "" || watch.TaskID == "" || watch.CWD == "" || watch.Command == "" {
		return nil, fmt.Errorf("person, task, cwd, and command are required")
	}
	if err := ValidateExternalWatchSpec(watch); err != nil {
		return nil, err
	}
	if watch.IntervalSeconds < 5 {
		watch.IntervalSeconds = 30
	}
	watch.CurrentIntervalSeconds = watch.IntervalSeconds
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
	receiptJSON, err := json.Marshal(watch.PreflightReceipt)
	if err != nil {
		return nil, fmt.Errorf("encode external watch preflight receipt: %w", err)
	}
	watch.Status = ExternalWatchPending
	watch.NextCheckAt = now
	watch.CreatedAt = now
	watch.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_watches (
		id, tenant_id, person_id, workspace_id, thread_id, run_id, channel,
		description, cwd, command, success_pattern, failure_pattern,
		spec_version, target_pattern, terminal_success_pattern, terminal_failure_pattern,
		observation_adapter, preflight_receipt_json, wait_group_id, status,
		interval_seconds, current_interval_seconds, command_timeout_seconds, timeout_at, next_check_at,
		attempts, last_output, last_output_hash, last_error,
		environment_snapshot_id, environment_generation, principal_fingerprint,
		environment_fingerprint, credential_source_hash, execution_binding_json,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', '', ?, ?, ?, ?, ?, ?, ?, ?)`,
		watch.ID, watch.TenantID, watch.PersonID, watch.WorkspaceID, watch.TaskID,
		watch.RunID, watch.Channel, watch.Description, watch.CWD, watch.Command,
		watch.SuccessPattern, watch.FailurePattern, watch.SpecVersion, watch.TargetPattern,
		watch.TerminalSuccessPattern, watch.TerminalFailurePattern, watch.ObservationAdapter, string(receiptJSON), watch.WaitGroupID, watch.Status,
		watch.IntervalSeconds, watch.CurrentIntervalSeconds, watch.CommandTimeoutSeconds, watch.TimeoutAt.Unix(),
		watch.NextCheckAt.Unix(),
		watch.EnvironmentSnapshotID, watch.EnvironmentGeneration, watch.PrincipalFingerprint,
		watch.EnvironmentFingerprint, watch.CredentialSourceHash, string(bindingJSON),
		now.Unix(), now.Unix()); err != nil {
		return nil, err
	}
	// A live watcher is durable work evidence: list the Run's Thread now
	// rather than only at finalization.
	if err := promoteThreadForControlObjectTx(ctx, tx, watch.TenantID, watch.RunID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &watch, nil
}

// ValidateExternalWatchSpec pins the durable verdict contract before a check
// is executed or stored. A desired intermediate target can be skipped by an
// external system, so target-based watches must also describe both terminal
// outcomes. Without those exits a successful or failed operation can only age
// into timed_out even though the check output is conclusive.
func ValidateExternalWatchSpec(watch ExternalWatch) error {
	if watch.SpecVersion >= 3 {
		if strings.TrimSpace(watch.ObservationAdapter) == "" {
			return fmt.Errorf("watch spec v3 requires an observation adapter")
		}
		return nil
	}
	if watch.SpecVersion <= 1 {
		if strings.TrimSpace(watch.SuccessPattern) == "" {
			return fmt.Errorf("success pattern is required for watch spec v1")
		}
		return nil
	}
	target := strings.TrimSpace(watch.TargetPattern)
	terminalSuccess := strings.TrimSpace(watch.TerminalSuccessPattern)
	terminalFailure := strings.TrimSpace(watch.TerminalFailurePattern)
	if target == "" && terminalSuccess == "" && terminalFailure == "" {
		return fmt.Errorf("watch spec v2 requires at least one target or terminal pattern")
	}
	if target != "" && (terminalSuccess == "" || terminalFailure == "") {
		return fmt.Errorf("watch spec v2 target_pattern requires both terminal_success_pattern and terminal_failure_pattern because the target may be skipped")
	}
	return nil
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

const (
	ExternalWatchListSummary   = "summary"
	ExternalWatchListActive    = "active"
	ExternalWatchListAttention = "attention"
	ExternalWatchListRecent    = "recent"
	ExternalWatchListAll       = "all"
)

// ListExternalWatchesForPerson is the human-facing watcher query. Ownership is
// enforced in SQL so a guessed watch id or a future multi-person tenant cannot
// leak another person's durable execution state.
func (s *Store) ListExternalWatchesForPerson(ctx context.Context, tenantID, personID, mode string, limit, offset int) ([]ExternalWatch, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	if limit <= 0 || limit > 50 {
		limit = 8
	}
	if offset < 0 {
		offset = 0
	}
	where := ""
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ExternalWatchListSummary, ExternalWatchListAll:
		// Summary/all share the same stable ordering; summary is only a smaller
		// page chosen by the caller.
	case ExternalWatchListActive:
		where = " AND status IN ('pending', 'running')"
	case ExternalWatchListAttention:
		where = ` AND (status IN ('failed', 'timed_out', 'blocked_environment')
			OR (status IN ('succeeded', 'failed', 'timed_out', 'blocked_environment')
				AND (COALESCE(finalized, 0) = 0 OR COALESCE(notified, 0) = 0)))`
	case ExternalWatchListRecent:
		where = " AND status IN ('succeeded', 'failed', 'timed_out', 'cancelled', 'blocked_environment')"
	default:
		return nil, fmt.Errorf("invalid external watch list mode %q", mode)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches
		WHERE tenant_id = ? AND person_id = ?`+where+`
		ORDER BY CASE
			WHEN status IN ('pending', 'running') THEN 0
			WHEN status IN ('failed', 'timed_out', 'blocked_environment') THEN 1
			WHEN COALESCE(finalized, 0) = 0 OR COALESCE(notified, 0) = 0 THEN 2
			ELSE 3 END,
			COALESCE(finished_at, updated_at) DESC, id ASC
		LIMIT ? OFFSET ?`, normalizeTenant(tenantID), strings.TrimSpace(personID), limit, offset)
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

// ResolveExternalWatchForPerson resolves a full id or a unique displayed
// prefix. It never falls back to a tenant-wide lookup.
func (s *Store) ResolveExternalWatchForPerson(ctx context.Context, tenantID, personID, ref string) (*ExternalWatch, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("control store is unavailable")
	}
	ref = strings.TrimSpace(strings.TrimSuffix(ref, "..."))
	if ref == "" {
		return nil, false, nil
	}
	if !strings.HasPrefix(ref, "watch_") {
		ref = "watch_" + ref
	}
	if len(ref) < len("watch_")+4 {
		return nil, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches
		WHERE tenant_id = ? AND person_id = ? AND substr(id, 1, length(?)) = ?
		ORDER BY CASE WHEN id = ? THEN 0 ELSE 1 END, updated_at DESC LIMIT 2`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), ref, ref, ref)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var found []ExternalWatch
	for rows.Next() {
		watch, err := scanExternalWatch(rows)
		if err != nil {
			return nil, false, err
		}
		found = append(found, watch)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(found) == 0 {
		return nil, false, nil
	}
	if found[0].ID == ref || len(found) == 1 {
		return &found[0], false, nil
	}
	return nil, true, nil
}

// CancelExternalWatchForPerson atomically cancels one owned active watcher.
// If it was the task's final live watcher, a waiting_external task is parked
// as waiting_user instead of being left with nothing capable of waking it.
func (s *Store) CancelExternalWatchForPerson(ctx context.Context, tenantID, personID, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	id = strings.TrimSpace(id)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var taskID, runID string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id, run_id FROM external_watches
		WHERE tenant_id = ? AND person_id = ? AND id = ?`, tenantID, personID, id).Scan(&taskID, &runID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE external_watches
		SET status = ?, last_error = ?, finalized = 1, notified = 1,
			finished_at = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND status IN (?, ?)`,
		ExternalWatchCancelled, "cancelled by user", now, now,
		tenantID, personID, id, ExternalWatchPending, ExternalWatchRunning)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads
		SET summary = COALESCE(NULLIF(summary, ''), 'External watcher cancelled by user.'),
			last_activity_at = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		now, now, tenantID, personID, taskID); err != nil {
		return false, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_watches
		WHERE tenant_id = ? AND person_id = ? AND run_id = ? AND status IN (?, ?)`,
		tenantID, personID, runID, ExternalWatchPending, ExternalWatchRunning).Scan(&active); err != nil {
		return false, err
	}
	if active == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE runs
			SET status = 'waiting_user', attention_dismissed_at = 0, attention_dismissed_by = ''
			WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'waiting_external'`,
			tenantID, personID, runID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkExternalWatchRunBlocked parks the Run that registered a watcher as
// `blocked` once that watcher's finalization has exhausted recovery. The Run is
// the execution truth Attention reads, so this is what makes an abandoned
// finalization visible and resumable; the Thread summary alone is not. Only a
// Run still parked as waiting_external with no live watcher left is changed:
// a Run that moved on, or that another watcher still serves, is untouched.
// The reason lands on the Run's last_error for diagnostics.
func (s *Store) MarkExternalWatchRunBlocked(ctx context.Context, tenantID, runID, reason string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("control store is unavailable")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false, nil
	}
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE runs
		SET status = 'blocked', last_error = ?, heartbeat_at = ?,
			attention_dismissed_at = 0, attention_dismissed_by = ''
		WHERE tenant_id = ? AND id = ? AND status = 'waiting_external'
		  AND NOT EXISTS (SELECT 1 FROM external_watches w
		                  WHERE w.tenant_id = runs.tenant_id AND w.run_id = runs.id AND w.status IN (?, ?))`,
		strings.TrimSpace(reason), now, normalizeTenant(tenantID), runID,
		ExternalWatchPending, ExternalWatchRunning)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// ListExternalWatchesFinishedSinceForPerson is the owner-scoped diagnostics
// variant. Daemon recovery intentionally uses the tenant-wide method above;
// user-visible diagnostics must not.
func (s *Store) ListExternalWatchesFinishedSinceForPerson(ctx context.Context, tenantID, personID, status string, since time.Time, limit int) ([]ExternalWatch, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+`
		FROM external_watches
		WHERE tenant_id = ? AND person_id = ? AND status = ?
			AND finished_at IS NOT NULL AND finished_at >= ?
		ORDER BY finished_at DESC LIMIT ?`, normalizeTenant(tenantID), strings.TrimSpace(personID), status, since.Unix(), limit)
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
	interval := watch.CurrentIntervalSeconds
	if interval < watch.IntervalSeconds {
		interval = watch.IntervalSeconds
	}
	next := now.Add(time.Duration(interval) * time.Second)
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
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var baseInterval, currentInterval int
	var previousHash string
	if err := tx.QueryRowContext(ctx, `SELECT interval_seconds,
		COALESCE(current_interval_seconds, 0), COALESCE(last_output_hash, '')
		FROM external_watches WHERE tenant_id = ? AND id = ? AND status = ?`,
		tenant, id, ExternalWatchRunning).Scan(&baseInterval, &currentInterval, &previousHash); err != nil {
		return err
	}
	if baseInterval < 5 {
		baseInterval = 30
	}
	if currentInterval < baseInterval {
		currentInterval = baseInterval
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(output))))
	nextInterval := baseInterval
	if previousHash != "" && previousHash == hash {
		nextInterval = currentInterval * 2
		if nextInterval > 60 {
			nextInterval = 60
		}
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE external_watches
		SET last_output = ?, last_output_hash = ?, last_error = ?, failure_class = '', check_signature = '',
			consecutive_failures = 0, current_interval_seconds = ?, next_check_at = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ?`,
		output, hash, lastError, nextInterval, now.Add(time.Duration(nextInterval)*time.Second).Unix(), now.Unix(),
		tenant, id, ExternalWatchRunning); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordExternalWatchPhases persists the independently observable layers of a
// watch. A checker failure must not erase a terminal operation result, and a
// successful operation must not imply that its post-deploy verification ran.
func (s *Store) RecordExternalWatchPhases(ctx context.Context, tenantID, id, checker, operation, verification string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE external_watches
		SET checker_status = ?, operation_status = ?, verification_status = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status IN (?, ?)`,
		strings.TrimSpace(checker), strings.TrimSpace(operation), strings.TrimSpace(verification),
		time.Now().Unix(), normalizeTenant(tenantID), id, ExternalWatchPending, ExternalWatchRunning)
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

// MarkExternalWatchNotified records that the completion has a confirmed user
// surface: either endpoint delivery was confirmed, or the durable completion
// event was published while that person's CLI was attached.
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
	var bindingJSON, receiptJSON string
	var timeoutAt, nextCheckAt, createdAt, updatedAt int64
	var finalized, notified int
	var finishedAt sql.NullInt64
	err := scanner.Scan(&watch.ID, &watch.TenantID, &watch.PersonID, &watch.WorkspaceID,
		&watch.TaskID, &watch.RunID, &watch.Channel, &watch.Description, &watch.CWD,
		&watch.Command, &watch.SuccessPattern, &watch.FailurePattern,
		&watch.SpecVersion, &watch.TargetPattern, &watch.TerminalSuccessPattern, &watch.TerminalFailurePattern,
		&watch.ObservationAdapter, &receiptJSON, &watch.WaitGroupID, &watch.Status,
		&watch.CheckerStatus, &watch.OperationStatus, &watch.VerificationStatus,
		&watch.IntervalSeconds, &watch.CurrentIntervalSeconds, &watch.CommandTimeoutSeconds, &timeoutAt, &nextCheckAt,
		&watch.Attempts, &watch.Extensions, &watch.VerdictRevision, &finalized, &notified, &watch.LastOutput, &watch.LastError,
		&watch.LastOutputHash,
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
	watch.Finalized = finalized != 0
	watch.Notified = notified != 0
	if err := json.Unmarshal([]byte(bindingJSON), &watch.ExecutionBinding); err != nil {
		return ExternalWatch{}, fmt.Errorf("decode external watch execution binding: %w", err)
	}
	if err := json.Unmarshal([]byte(receiptJSON), &watch.PreflightReceipt); err != nil {
		return ExternalWatch{}, fmt.Errorf("decode external watch preflight receipt: %w", err)
	}
	if finishedAt.Valid {
		value := time.Unix(finishedAt.Int64, 0)
		watch.FinishedAt = &value
	}
	return watch, nil
}
