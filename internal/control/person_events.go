package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ListPersonEventsSince returns bounded durable events for local diagnostics
// and quality reports. It preserves ownership isolation through the tasks
// join and never exposes another person's task stream.
func (s *Store) ListPersonEventsSince(ctx context.Context, tenantID, personID string, since time.Time, limit int) ([]Event, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	out, err := s.listPersonEventsSincePage(ctx, tenantID, personID, since, 0, limit)
	if err != nil {
		return nil, err
	}
	// The SQL limit keeps the most recent diagnostic window. Restore timeline
	// order for reducers so completion and correction events are processed in
	// the same direction as the live event stream.
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out, nil
}

// ListPersonEventsSincePage returns one newest-first cursor page. beforeCursor
// is exclusive; zero starts at the newest event. Long diagnostic reports use
// this instead of silently truncating at one fixed SQL limit.
func (s *Store) ListPersonEventsSincePage(ctx context.Context, tenantID, personID string, since time.Time, beforeCursor int64, limit int) ([]Event, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	return s.listPersonEventsSincePage(ctx, tenantID, personID, since, beforeCursor, limit)
}

func (s *Store) listPersonEventsSincePage(ctx context.Context, tenantID, personID string, since time.Time, beforeCursor int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.cursor, e.id, e.task_id, COALESCE(e.run_id, ''), e.type, e.visibility,
		        COALESCE(e.channel, ''), COALESCE(e.payload_json, '{}'), e.created_at,
		        t.tenant_id, t.person_id
		 FROM task_events e JOIN tasks t ON t.id = e.task_id
		 WHERE t.tenant_id = ? AND t.person_id = ? AND e.created_at >= ?
		   AND (? <= 0 OR e.cursor < ?)
		 ORDER BY e.cursor DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, since.Unix(), beforeCursor, beforeCursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		var created int64
		if err := rows.Scan(&e.Cursor, &e.ID, &e.TaskID, &e.RunID, &e.Type, &e.Visibility,
			&e.Channel, &payload, &created, &e.TenantID, &e.PersonID); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt = time.Unix(created, 0)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
