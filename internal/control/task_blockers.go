package control

import (
	"context"
	"database/sql"
	"time"
)

// resolveOriginRunBlockersTx settles LEGACY task_blockers rows. New rows are
// no longer created (simplification §10.3: unclaimed resumable runs are the
// wait authority), but databases upgraded from the blocker era may still hold
// open rows; marking them resolved when their origin run is claimed keeps the
// historical record coherent.
func resolveOriginRunBlockersTx(ctx context.Context, tx *sql.Tx, tenantID, taskID, originRunID, resolvedByRunID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE task_blockers
		SET status = 'resolved', resolved_by_run_id = ?, resolved_at = ?
		WHERE tenant_id = ? AND task_id = ? AND origin_run_id = ? AND status = 'open'`,
		resolvedByRunID, time.Now().Unix(), tenantID, taskID, originRunID)
	return err
}
