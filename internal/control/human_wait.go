package control

import (
	"context"
	"fmt"
	"time"
)

// ParkHumanWaitRun releases execution only with this exact Run's bound,
// incomplete checkpoint. The request and Run transition in one transaction;
// an answer that won the race is left for the current waiter to consume.
func (s *Store) ParkHumanWaitRun(ctx context.Context, tenantID, personID, runID, kind, ref string) (bool, error) {
	tenantID = normalizeTenant(tenantID)
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
	switch kind {
	case LoopCheckpointWaitApproval:
		query = `UPDATE approval_requests SET waiter_state='parked', parked_at=?, park_reason='human wait grace elapsed', updated_at=?
   WHERE tenant_id=? AND person_id=? AND run_id=? AND id=? AND status='pending' AND COALESCE(waiter_state,'live')='live'`
	case LoopCheckpointWaitClarification:
		query = `UPDATE clarify_requests SET parked_at=?, updated_at=? WHERE tenant_id=? AND person_id=? AND run_id=? AND id=? AND status='pending' AND parked_at=0`
	default:
		return false, fmt.Errorf("unknown human wait kind %q", kind)
	}
	result, err := tx.ExecContext(ctx, query, now, now, tenantID, personID, runID, ref)
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
