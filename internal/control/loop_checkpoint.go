package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Typed waits bind a checkpoint to a human request or an uninvoked execution.
const (
	LoopCheckpointWaitExecutionLane     = "execution_lane"
	LoopCheckpointWaitExecutionHostLane = "execution_host_lane"
	LoopCheckpointWaitApproval          = "approval"
	LoopCheckpointWaitClarification     = "clarification"
)

// ErrLoopCheckpointWaitBinding reports a wait binding that is incomplete, of an
// unknown kind, or names a request that is not this Run's.
var ErrLoopCheckpointWaitBinding = errors.New("loop checkpoint wait binding is not this run's wait")

// LoopCheckpointRecord is storage-neutral checkpoint data. Snapshot contains
// the kernel message ledger encoded by the gateway, keeping control free of a
// dependency on kernel/llm types.
type LoopCheckpointRecord struct {
	TenantID        string
	PersonID        string
	TaskID          string
	RunID           string
	ContractVersion int
	Recovery        []byte
	Iteration       int
	Outcome         string
	Detail          string
	Snapshot        []byte
	UpdatedAt       time.Time
	// WaitKind, WaitRef, and WaitToolCallID bind a checkpoint to its exact
	// human request or execution wait and the pending tool call. A binding is
	// all three or none; none keeps today's
	// soft resume, which is what every checkpoint saved before schema v26 has.
	WaitKind       string
	WaitRef        string
	WaitToolCallID string
}

// loopCheckpointToolCallMaxBytes bounds the tool call id a wait binding names.
const loopCheckpointToolCallMaxBytes = 256

func (s *Store) SaveLoopCheckpoint(ctx context.Context, record LoopCheckpointRecord) error {
	tenantID := normalizeTenant(record.TenantID)
	record.WaitKind = strings.TrimSpace(record.WaitKind)
	record.WaitRef = strings.TrimSpace(record.WaitRef)
	record.WaitToolCallID = strings.TrimSpace(strings.ToValidUTF8(record.WaitToolCallID, ""))
	if err := s.checkLoopCheckpointWait(ctx, tenantID, record); err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO loop_checkpoints
		(run_id, tenant_id, person_id, thread_id, contract_version, recovery_json, iteration, outcome, detail, snapshot_json, updated_at,
		 wait_kind, wait_ref, wait_tool_call_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			contract_version = excluded.contract_version,
			recovery_json = excluded.recovery_json,
			iteration = excluded.iteration,
			outcome = excluded.outcome,
			detail = excluded.detail,
			snapshot_json = excluded.snapshot_json,
			updated_at = excluded.updated_at,
			wait_kind = excluded.wait_kind,
			wait_ref = excluded.wait_ref,
			wait_tool_call_id = excluded.wait_tool_call_id`,
		record.RunID, tenantID, record.PersonID, record.TaskID,
		record.ContractVersion, recoveryJSON(record.Recovery), record.Iteration, record.Outcome, record.Detail, record.Snapshot, now,
		record.WaitKind, record.WaitRef, record.WaitToolCallID)
	return err
}

// checkLoopCheckpointWait accepts no binding, or a complete binding to an
// human request or execution wait of the same person and Run. The tool call id is
// bounded, and where the approval records the call that asked, it must match.
func (s *Store) checkLoopCheckpointWait(ctx context.Context, tenantID string, record LoopCheckpointRecord) error {
	if record.WaitKind == "" && record.WaitRef == "" && record.WaitToolCallID == "" {
		return nil
	}
	if record.WaitRef == "" || record.WaitToolCallID == "" {
		return fmt.Errorf("%w: a binding needs its request and tool call", ErrLoopCheckpointWaitBinding)
	}
	if len(record.WaitToolCallID) > loopCheckpointToolCallMaxBytes {
		return fmt.Errorf("%w: tool call id exceeds %d bytes", ErrLoopCheckpointWaitBinding, loopCheckpointToolCallMaxBytes)
	}
	var query string
	switch record.WaitKind {
	case LoopCheckpointWaitExecutionLane, LoopCheckpointWaitExecutionHostLane:
		if record.Outcome != "await_execution" || record.WaitRef != record.RunID {
			return fmt.Errorf("%w: execution wait must name this Run", ErrLoopCheckpointWaitBinding)
		}
		query = `SELECT '' FROM runs WHERE tenant_id = ? AND id = ? AND person_id = ? AND id = ? AND status = 'running'`
	case LoopCheckpointWaitApproval:
		query = `SELECT CASE WHEN json_valid(COALESCE(payload_json, ''))
			THEN COALESCE(json_extract(payload_json, '$.tool_call_id'), '') ELSE '' END
			FROM approval_requests WHERE tenant_id = ? AND id = ? AND person_id = ? AND run_id = ?`
	case LoopCheckpointWaitClarification:
		query = `SELECT '' FROM clarify_requests WHERE tenant_id = ? AND id = ? AND person_id = ? AND run_id = ?`
	default:
		return fmt.Errorf("%w: unknown wait kind %q", ErrLoopCheckpointWaitBinding, record.WaitKind)
	}
	var recordedCall string
	err := s.db.QueryRowContext(ctx, query, tenantID, record.WaitRef, record.PersonID, record.RunID).Scan(&recordedCall)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s %s", ErrLoopCheckpointWaitBinding, record.WaitKind, record.WaitRef)
	}
	if err != nil {
		return err
	}
	if recordedCall != "" && recordedCall != record.WaitToolCallID {
		return fmt.Errorf("%w: %s %s asked from another tool call", ErrLoopCheckpointWaitBinding, record.WaitKind, record.WaitRef)
	}
	return nil
}

// IncompleteLoopCheckpointForRun returns exactly one run's incomplete
// checkpoint, or nil. Continuations restore a checkpoint only through their
// resolved parent run; the task-wide most-recent pick remains only for
// legacy callers and must not gain new ones.
func (s *Store) IncompleteLoopCheckpointForRun(ctx context.Context, tenantID, runID string) (*LoopCheckpointRecord, error) {
	if runID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+loopCheckpointColumns+`
		FROM loop_checkpoints
		WHERE tenant_id = ? AND run_id = ? AND outcome <> 'complete_turn'
		LIMIT 1`, normalizeTenant(tenantID), runID)
	return scanLoopCheckpoint(row)
}

// CompletedLoopCheckpointForRun returns an exact completed turn's durable
// ledger for bounded continuation selection. It is never treated as a live
// loop checkpoint: callers select excerpts instead of replaying the full
// message array into another Run.
func (s *Store) CompletedLoopCheckpointForRun(ctx context.Context, tenantID, personID, runID string) (*LoopCheckpointRecord, error) {
	if s == nil || s.db == nil || runID == "" || personID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+loopCheckpointColumns+`
		FROM loop_checkpoints WHERE tenant_id=? AND person_id=? AND run_id=? AND outcome='complete_turn'
		LIMIT 1`, normalizeTenant(tenantID), personID, runID)
	return scanLoopCheckpoint(row)
}

const loopCheckpointColumns = `tenant_id, person_id, thread_id, run_id,
	contract_version, recovery_json, iteration, outcome, detail, snapshot_json, updated_at,
	wait_kind, wait_ref, wait_tool_call_id`

func scanLoopCheckpoint(row *sql.Row) (*LoopCheckpointRecord, error) {
	var record LoopCheckpointRecord
	var updated int64
	if err := row.Scan(&record.TenantID, &record.PersonID, &record.TaskID, &record.RunID,
		&record.ContractVersion, &record.Recovery, &record.Iteration, &record.Outcome, &record.Detail, &record.Snapshot, &updated,
		&record.WaitKind, &record.WaitRef, &record.WaitToolCallID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	record.UpdatedAt = time.Unix(updated, 0)
	return &record, nil
}

func recoveryJSON(value []byte) []byte {
	if len(value) == 0 {
		return []byte("{}")
	}
	return value
}
