package control

import (
	"context"
	"database/sql"
)

// Historical identities stay reserved even when no longer in the latest
// snapshot. Allocation and insertion share the plan's existing transaction.
func runPlanStepIDsTx(ctx context.Context, tx *sql.Tx, tenant, runID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT step_id FROM run_plan_steps WHERE tenant_id=? AND run_id=?`, tenant, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}
