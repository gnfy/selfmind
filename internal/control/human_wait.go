package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Why a human wait released its worker. The value is stored in
// approval_requests.park_reason so a later reader can tell a park the person's
// own slowness caused from one the runtime forced.
const (
	// HumanWaitParkGrace is the short-grace park: no in-place hold applied, or
	// the hold was never armed.
	HumanWaitParkGrace = "grace"
	// HumanWaitParkHoldCap: the wait stayed in place for its whole cap.
	HumanWaitParkHoldCap = "hold_cap"
	// HumanWaitParkCapacity: other work of the same person needed the slot.
	HumanWaitParkCapacity = "capacity"
	// HumanWaitParkDraining: the daemon began a restart or model-change drain.
	HumanWaitParkDraining = "draining"
	// HumanWaitParkClientDetached: no live terminal remained attached.
	HumanWaitParkClientDetached = "client_detached"
)

// ParkHumanWaitRun releases execution only with this exact Run's bound,
// incomplete checkpoint. The request and Run transition in one transaction;
// an answer that won the race is left for the current waiter to consume.
// reason is one of the HumanWaitPark* values; an approval stores it, a
// clarification has no column for it.
func (s *Store) ParkHumanWaitRun(ctx context.Context, tenantID, personID, runID, kind, ref, reason string) (bool, error) {
	tenantID = normalizeTenant(tenantID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = HumanWaitParkGrace
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var bound bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loop_checkpoints c JOIN runs r ON r.id=c.run_id AND r.tenant_id=c.tenant_id
 WHERE c.tenant_id=? AND c.person_id=? AND c.run_id=? AND c.outcome='await_human' AND c.wait_kind=? AND c.wait_ref=? AND c.wait_tool_call_id<>'' AND r.person_id=? AND r.status='running' AND COALESCE(r.cancel_requested,0)=0)`, tenantID, personID, runID, kind, ref, personID).Scan(&bound); err != nil {
		return false, err
	}
	if !bound {
		return false, fmt.Errorf("human wait has no exact incomplete checkpoint on a running Run")
	}
	now := time.Now().Unix()
	var query string
	var args []any
	switch kind {
	case LoopCheckpointWaitApproval:
		query = `UPDATE approval_requests SET waiter_state='parked', parked_at=?, park_reason=?, updated_at=?
   WHERE tenant_id=? AND person_id=? AND run_id=? AND id=? AND status='pending' AND COALESCE(waiter_state,'live')='live'`
		args = []any{now, reason, now, tenantID, personID, runID, ref}
	case LoopCheckpointWaitClarification:
		query = `UPDATE clarify_requests SET parked_at=?, updated_at=? WHERE tenant_id=? AND person_id=? AND run_id=? AND id=? AND status='pending' AND parked_at=0`
		args = []any{now, now, tenantID, personID, runID, ref}
	default:
		return false, fmt.Errorf("unknown human wait kind %q", kind)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return false, nil
	}
	result, err = tx.ExecContext(ctx, `UPDATE runs SET status='waiting_user', finished_at=?, heartbeat_at=? WHERE tenant_id=? AND person_id=? AND id=? AND status='running'`, now, now, tenantID, personID, runID)
	if err != nil {
		return false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return false, fmt.Errorf("human wait Run changed while parking")
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// WaitRelease says what a live waiter puts back into its Run's loop checkpoint
// when it consumes the answer in place. A wait that stays in its Run writes the
// exact pending-call checkpoint when it starts, so a crash or a park recovers
// exactly; once the answer is consumed that checkpoint no longer describes
// anything pending, and a crash while the approved call runs must not let a
// later resume dispatch it again. Prior is the checkpoint the wait overwrote;
// nil removes the binding and leaves the Run with none.
type WaitRelease struct {
	Prior *LoopCheckpointRecord
}

type waitExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ReleaseHumanWait ends a live wait's checkpoint binding without parking it.
// It acts only on the checkpoint still bound to this exact request of a Run that
// is not parked. A parked wait keeps its binding (its Run is waiting_user by
// then), so it is safe to call after the binding is gone or became a park.
func (s *Store) ReleaseHumanWait(ctx context.Context, tenantID, personID, runID, kind, ref string, release WaitRelease) error {
	return releaseHumanWait(ctx, s.db, tenantID, personID, runID, kind, ref, release)
}

// The Run must not be parked: a Run parked in the meantime keeps the exact
// checkpoint its answer will resume. Every other status leaves the binding with
// no waiter. A stop or a forced stop finalizes the Run without waiting for the
// wait's handler to unwind, so the handler's release can arrive after the Run
// ended, and it must still clear what that wait wrote.
const liveHumanWaitBinding = ` WHERE tenant_id=? AND person_id=? AND run_id=? AND outcome='await_human' AND wait_kind=? AND wait_ref=?
 AND EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id=loop_checkpoints.tenant_id AND r.id=loop_checkpoints.run_id AND r.status<>'waiting_user')`

func releaseHumanWait(ctx context.Context, db waitExecer, tenantID, personID, runID, kind, ref string, release WaitRelease) error {
	tenantID = normalizeTenant(tenantID)
	personID, runID, kind, ref = strings.TrimSpace(personID), strings.TrimSpace(runID), strings.TrimSpace(kind), strings.TrimSpace(ref)
	if personID == "" || runID == "" || kind == "" || ref == "" {
		return fmt.Errorf("releasing a human wait requires its person, Run and request")
	}
	prior := release.Prior
	if prior == nil {
		_, err := db.ExecContext(ctx, `DELETE FROM loop_checkpoints`+liveHumanWaitBinding,
			tenantID, personID, runID, kind, ref)
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE loop_checkpoints SET contract_version=?, recovery_json=?, iteration=?, outcome=?, detail=?,
 snapshot_json=?, updated_at=?, wait_kind=?, wait_ref=?, wait_tool_call_id=?`+liveHumanWaitBinding,
		prior.ContractVersion, recoveryJSON(prior.Recovery), prior.Iteration, prior.Outcome, prior.Detail,
		prior.Snapshot, time.Now().Unix(), prior.WaitKind, prior.WaitRef, prior.WaitToolCallID,
		tenantID, personID, runID, kind, ref)
	return err
}
