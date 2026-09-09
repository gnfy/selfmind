package control

import (
	"context"
	"encoding/json"
	"time"
)

// ListRunEvidenceEvents pages execution evidence independently of transcript
// volume. The owning Run supplies tenant identity, including threadless Runs.
func (s *Store) ListRunEvidenceEvents(ctx context.Context, tenantID, runID string, before int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.cursor,e.id,e.thread_id,e.run_id,e.payload_json,e.created_at
		FROM task_events e JOIN runs r ON r.id=e.run_id
		WHERE r.tenant_id=? AND r.id=? AND e.type='evidence.recorded'
		AND (?=0 OR e.cursor<?) ORDER BY e.cursor DESC LIMIT ?`, normalizeTenant(tenantID), runID, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var payload string
		var created int64
		if err := rows.Scan(&event.Cursor, &event.ID, &event.TaskID, &event.RunID, &payload, &created); err != nil {
			return nil, err
		}
		event.Type = "evidence.recorded"
		event.Payload = json.RawMessage(payload)
		event.CreatedAt = time.Unix(created, 0)
		events = append(events, event)
	}
	return events, rows.Err()
}
