package control

import (
	"context"
	"database/sql"
	"encoding/json"
)

// RunWorkSelection reads the latest typed selection, independently of noisy
// progress events. It is owned by the exact person and Run, not a Thread tail.
func (s *Store) RunWorkSelection(ctx context.Context, tenantID, personID, runID string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT e.payload_json FROM task_events e
 JOIN runs r ON r.id=e.run_id AND r.thread_id=e.thread_id
 WHERE r.tenant_id=? AND r.person_id=? AND r.id=? AND e.type='work.selection'
 ORDER BY e.cursor DESC LIMIT 1`, normalizeTenant(tenantID), personID, runID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return json.RawMessage(raw), err
}
