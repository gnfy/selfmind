package control

import (
	"context"
	"database/sql"
	"encoding/json"
)

// RunApprovalIntent returns only the server-recorded policy evidence for an
// exact owned Run. It never falls back to model prose or another Thread.
func (s *Store) RunApprovalIntent(ctx context.Context, tenantID, personID, runID string) (json.RawMessage, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT json_extract(e.payload_json, '$.approval_intent')
		FROM task_events e JOIN runs r ON r.id=e.run_id
		WHERE r.tenant_id=? AND r.person_id=? AND r.id=? AND e.type='run.started'
		ORDER BY e.rowid ASC LIMIT 1`, normalizeTenant(tenantID), personID, runID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw.String), nil
}
