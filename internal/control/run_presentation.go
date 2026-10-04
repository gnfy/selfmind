package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// RunStartedPayload reads the exact Run's durable presentation facts. It does
// not infer ownership from a Thread title or from generated continuation text.
func (s *Store) RunStartedPayload(ctx context.Context, tenant, person, runID string) (json.RawMessage, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT e.payload_json FROM task_events e
	 JOIN runs r ON r.id=e.run_id AND r.tenant_id=? AND r.person_id=?
	 WHERE e.run_id=? AND e.type='run.started' ORDER BY e.cursor LIMIT 1`, tenant, person, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return json.RawMessage(payload), err
}
