package control

import (
	"context"
	"database/sql"
)

// The wait row, exact effect id and durable queue must agree. A key prefix or
// historical waiting_external Run alone cannot acquire cancellation capability.
const resourceWakeQueueCondition = `q.class='finalization' AND EXISTS (
	SELECT 1 FROM external_resource_waits w
	WHERE w.tenant_id=r.tenant_id AND w.person_id=r.person_id AND w.run_id=r.id AND w.status='queued'
	  AND q.idempotency_key='external-resource:'||r.id||':'||w.effect_id)`

func queuedResourceWaitParentTx(ctx context.Context, tx *sql.Tx, tenantID, personID, queueID string) (string, error) {
	var parent string
	err := tx.QueryRowContext(ctx, `SELECT r.id FROM runs r JOIN task_queue q
		ON q.tenant_id=r.tenant_id AND q.person_id=r.person_id AND q.reply_to_run_id=r.id
		WHERE q.tenant_id=? AND q.person_id=? AND q.id=? AND q.status IN ('queued','started')
		AND COALESCE(q.run_id,'')='' AND `+resourceWakeQueueCondition+` AND `+parkedProviderCondition,
		tenantID, personID, queueID).Scan(&parent)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return parent, err
}
