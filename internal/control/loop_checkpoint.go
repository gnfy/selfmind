package control

import (
	"context"
	"database/sql"
	"time"
)

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
}

func (s *Store) SaveLoopCheckpoint(ctx context.Context, record LoopCheckpointRecord) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO loop_checkpoints
		(run_id, tenant_id, person_id, task_id, contract_version, recovery_json, iteration, outcome, detail, snapshot_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			contract_version = excluded.contract_version,
			recovery_json = excluded.recovery_json,
			iteration = excluded.iteration,
			outcome = excluded.outcome,
			detail = excluded.detail,
			snapshot_json = excluded.snapshot_json,
			updated_at = excluded.updated_at`,
		record.RunID, normalizeTenant(record.TenantID), record.PersonID, record.TaskID,
		record.ContractVersion, recoveryJSON(record.Recovery), record.Iteration, record.Outcome, record.Detail, record.Snapshot, now)
	return err
}

// IncompleteLoopCheckpointForRun returns exactly one run's incomplete
// checkpoint, or nil. Continuations restore a checkpoint only through their
// resolved parent run; the task-wide most-recent pick remains only for
// legacy callers and must not gain new ones.
func (s *Store) IncompleteLoopCheckpointForRun(ctx context.Context, tenantID, runID string) (*LoopCheckpointRecord, error) {
	if runID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT tenant_id, person_id, task_id, run_id,
		contract_version, recovery_json, iteration, outcome, detail, snapshot_json, updated_at
		FROM loop_checkpoints
		WHERE tenant_id = ? AND run_id = ? AND outcome <> 'complete_turn'
		LIMIT 1`, normalizeTenant(tenantID), runID)
	var record LoopCheckpointRecord
	var updated int64
	if err := row.Scan(&record.TenantID, &record.PersonID, &record.TaskID, &record.RunID,
		&record.ContractVersion, &record.Recovery, &record.Iteration, &record.Outcome, &record.Detail, &record.Snapshot, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	record.UpdatedAt = time.Unix(updated, 0)
	return &record, nil
}

func (s *Store) LatestIncompleteLoopCheckpoint(ctx context.Context, tenantID, taskID, excludeRunID string) (*LoopCheckpointRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT tenant_id, person_id, task_id, run_id,
		contract_version, recovery_json, iteration, outcome, detail, snapshot_json, updated_at
		FROM loop_checkpoints
		WHERE tenant_id = ? AND task_id = ? AND run_id <> ?
		  AND outcome <> 'complete_turn'
		ORDER BY updated_at DESC, run_id DESC LIMIT 1`, normalizeTenant(tenantID), taskID, excludeRunID)
	var record LoopCheckpointRecord
	var updated int64
	if err := row.Scan(&record.TenantID, &record.PersonID, &record.TaskID, &record.RunID,
		&record.ContractVersion, &record.Recovery, &record.Iteration, &record.Outcome, &record.Detail, &record.Snapshot, &updated); err != nil {
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
