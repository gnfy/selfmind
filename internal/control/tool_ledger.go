package control

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Tool execution ledger (Loop Engineering P0-B). One row per dispatched tool
// call. The safety-critical transition is dispatched → completed: a crash in
// between leaves a durable `dispatched` row whose outcome is UNCERTAIN. Boot
// recovery reads uncertain side-effect entries and drives verification rather
// than a blind re-run.
const (
	ToolLedgerPlanned = "planned"
	ToolLedgerStarted = "started"
	// ToolLedgerDispatched is the legacy name for a started call. Existing
	// databases may still contain it, so recovery treats it as uncertain.
	ToolLedgerDispatched = "dispatched"
	ToolLedgerCompleted  = "completed"
	ToolLedgerFailed     = "failed"
)

// ToolLedgerRetention bounds the completed/failed history; only the tiny set
// of uncertain rows matters for correctness, the rest is diagnostic.
const ToolLedgerRetention = 7 * 24 * time.Hour

type ToolLedgerEntry struct {
	RunID                 string
	ToolCallID            string
	ToolName              string
	ArgsHash              string
	RetryClass            string
	EffectID              string
	PlanVersion           int
	PlanStepID            string
	Strategy              string
	EffectClass           string
	EnvironmentGeneration int64
	ResultRef             string
	VerificationState     string
	Status                string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// ToolDispatchClaim is the durable decision made before a tool may execute.
// Execute is true for exactly one claimant. Duplicate calls return the
// existing status without regressing or replaying the tool.
type ToolDispatchClaim struct {
	Execute bool
	Status  string
}

// ClaimToolDispatch atomically creates and claims one tool execution slot.
// The transition is monotonic:
//
//	missing -> planned -> started -> completed|failed
//
// A crash after started is deliberately uncertain. A caller seeing started
// (or the legacy dispatched state) must verify external state, never replay.
func (s *Store) ClaimToolDispatch(ctx context.Context, tenantID string, e ToolLedgerEntry) (ToolDispatchClaim, error) {
	if s == nil || s.db == nil {
		return ToolDispatchClaim{}, fmt.Errorf("control store is unavailable")
	}
	e.RunID = strings.TrimSpace(e.RunID)
	e.ToolCallID = strings.TrimSpace(e.ToolCallID)
	e.ToolName = strings.TrimSpace(e.ToolName)
	if e.RunID == "" || e.ToolCallID == "" || e.ToolName == "" {
		return ToolDispatchClaim{}, fmt.Errorf("run id, tool call id, and tool name are required")
	}
	tenantID = normalizeTenant(tenantID)
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ToolDispatchClaim{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_ledger
		(tenant_id, run_id, tool_call_id, tool_name, args_hash, retry_class, effect_id, plan_version,
		 plan_step_id, strategy, effect_class, environment_generation, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, tool_call_id) DO NOTHING`,
		tenantID, e.RunID, e.ToolCallID, e.ToolName, e.ArgsHash,
		e.RetryClass, e.EffectID, e.PlanVersion, e.PlanStepID, e.Strategy, e.EffectClass,
		e.EnvironmentGeneration, ToolLedgerPlanned, now, now); err != nil {
		return ToolDispatchClaim{}, err
	}
	var toolName, argsHash, retryClass, effectID, planStepID, strategy, effectClass, status string
	var planVersion int
	var environmentGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT tool_name, args_hash, retry_class, effect_id, plan_version,
		plan_step_id, strategy, effect_class, environment_generation, status
		FROM tool_ledger WHERE tenant_id = ? AND run_id = ? AND tool_call_id = ?`,
		tenantID, e.RunID, e.ToolCallID).Scan(&toolName, &argsHash, &retryClass, &effectID, &planVersion,
		&planStepID, &strategy, &effectClass, &environmentGeneration, &status); err != nil {
		return ToolDispatchClaim{}, err
	}
	if toolName != e.ToolName || argsHash != e.ArgsHash || retryClass != e.RetryClass ||
		effectID != e.EffectID || planVersion != e.PlanVersion || planStepID != e.PlanStepID ||
		strategy != e.Strategy || effectClass != e.EffectClass || environmentGeneration != e.EnvironmentGeneration {
		return ToolDispatchClaim{}, fmt.Errorf("tool call id %s was already claimed with different arguments", e.ToolCallID)
	}
	if status != ToolLedgerPlanned {
		if err := tx.Commit(); err != nil {
			return ToolDispatchClaim{}, err
		}
		return ToolDispatchClaim{Status: status}, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE tool_ledger SET status = ?, updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND tool_call_id = ? AND status = ?`,
		ToolLedgerStarted, now, tenantID, e.RunID, e.ToolCallID, ToolLedgerPlanned)
	if err != nil {
		return ToolDispatchClaim{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ToolDispatchClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return ToolDispatchClaim{}, err
	}
	if n != 1 {
		return ToolDispatchClaim{Status: ToolLedgerStarted}, nil
	}
	return ToolDispatchClaim{Execute: true, Status: ToolLedgerStarted}, nil
}

// RecordToolDispatch inserts the pre-execution row. Idempotent on
// (run_id, tool_call_id). Kept as a compatibility helper for recovery tests
// and older callers; it never regresses terminal state.
func (s *Store) RecordToolDispatch(ctx context.Context, tenantID string, e ToolLedgerEntry) error {
	_, err := s.ClaimToolDispatch(ctx, tenantID, e)
	return err
}

// RecordToolOutcome closes the uncertain window after execution returns.
func (s *Store) RecordToolOutcome(ctx context.Context, tenantID, runID, toolCallID string, ok bool) error {
	return s.RecordToolOutcomeWithRef(ctx, tenantID, runID, toolCallID, ok, "")
}

func (s *Store) RecordToolOutcomeWithRef(ctx context.Context, tenantID, runID, toolCallID string, ok bool, resultRef string) error {
	status := ToolLedgerCompleted
	if !ok {
		status = ToolLedgerFailed
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tool_ledger SET status = ?, result_ref = ?, verification_state = 'recorded', updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND tool_call_id = ? AND status IN (?, ?)`,
		status, strings.TrimSpace(resultRef), time.Now().Unix(), normalizeTenant(tenantID), runID, toolCallID,
		ToolLedgerStarted, ToolLedgerDispatched)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var existing string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM tool_ledger
		WHERE tenant_id = ? AND run_id = ? AND tool_call_id = ?`,
		normalizeTenant(tenantID), runID, toolCallID).Scan(&existing); err != nil {
		return err
	}
	if existing == status {
		return nil
	}
	return fmt.Errorf("tool outcome transition rejected: %s -> %s", existing, status)
}

// ListUncertainToolEntries returns dispatched-but-unresolved side-effectful
// entries for a run — the exact set a resume must verify before repeating.
// Read-only entries are excluded (a blind re-run of those is safe and free).
func (s *Store) ListUncertainToolEntries(ctx context.Context, tenantID, runID string, limit int) ([]ToolLedgerEntry, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, tool_call_id, tool_name, args_hash, retry_class,
		effect_id, plan_version, plan_step_id, strategy, effect_class, environment_generation,
		result_ref, verification_state, status, created_at, updated_at
		FROM tool_ledger
		WHERE tenant_id = ? AND run_id = ? AND status IN (?, ?) AND retry_class != ?
		ORDER BY created_at ASC LIMIT ?`,
		normalizeTenant(tenantID), runID, ToolLedgerStarted, ToolLedgerDispatched, "read_only", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolLedgerEntry
	for rows.Next() {
		var e ToolLedgerEntry
		var created, updated int64
		if err := rows.Scan(&e.RunID, &e.ToolCallID, &e.ToolName, &e.ArgsHash, &e.RetryClass,
			&e.EffectID, &e.PlanVersion, &e.PlanStepID, &e.Strategy, &e.EffectClass,
			&e.EnvironmentGeneration, &e.ResultRef, &e.VerificationState, &e.Status, &created, &updated); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0)
		e.UpdatedAt = time.Unix(updated, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListUncertainToolEntriesForTask returns uncertain side-effect entries across
// ALL of a task's runs (a resume starts a fresh run, so the crashed run's
// entries hang off a different run_id). Joins the ledger to task_runs by task.
func (s *Store) ListUncertainToolEntriesForTask(ctx context.Context, tenantID, taskID string, limit int) ([]ToolLedgerEntry, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.run_id, l.tool_call_id, l.tool_name, l.args_hash, l.retry_class,
		l.effect_id, l.plan_version, l.plan_step_id, l.strategy, l.effect_class, l.environment_generation,
		l.result_ref, l.verification_state, l.status, l.created_at, l.updated_at
		FROM tool_ledger l JOIN task_runs r ON r.id = l.run_id AND r.tenant_id = l.tenant_id
		WHERE l.tenant_id = ? AND r.task_id = ? AND l.status IN (?, ?) AND l.retry_class != ?
		ORDER BY l.created_at ASC LIMIT ?`,
		normalizeTenant(tenantID), taskID, ToolLedgerStarted, ToolLedgerDispatched, "read_only", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolLedgerEntry
	for rows.Next() {
		var e ToolLedgerEntry
		var created, updated int64
		if err := rows.Scan(&e.RunID, &e.ToolCallID, &e.ToolName, &e.ArgsHash, &e.RetryClass,
			&e.EffectID, &e.PlanVersion, &e.PlanStepID, &e.Strategy, &e.EffectClass,
			&e.EnvironmentGeneration, &e.ResultRef, &e.VerificationState, &e.Status, &created, &updated); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0)
		e.UpdatedAt = time.Unix(updated, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneToolLedger drops resolved (completed/failed) rows past the retention
// window at worker/boot time. Uncertain `dispatched` rows are never pruned:
// they are correctness state until a resume verifies them.
func (s *Store) PruneToolLedger(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		olderThan = ToolLedgerRetention
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM tool_ledger
		WHERE status IN (?, ?) AND updated_at < ?`,
		ToolLedgerCompleted, ToolLedgerFailed, time.Now().Add(-olderThan).Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
