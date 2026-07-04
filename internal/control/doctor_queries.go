package control

// Bounded, read-only control-plane queries backing the observability export
// (`selfmind doctor` / `/diag`). All are person-scoped snapshots; none mutate
// state, so they are safe to run against a live daemon's control.db.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RunDigest is one recent run with its task title, for the doctor "recent runs"
// section. It joins task_runs to tasks so the operator sees what each run was.
type RunDigest struct {
	RunID      string     `json:"run_id"`
	TaskID     string     `json:"task_id"`
	TaskTitle  string     `json:"task_title"`
	Status     string     `json:"status"`
	Channel    string     `json:"channel"`
	LastError  string     `json:"last_error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Elapsed reports the run's wall-clock duration: to FinishedAt if terminal,
// otherwise to now (a still-running run).
func (r RunDigest) Elapsed() time.Duration {
	end := time.Now()
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	if r.StartedAt.IsZero() || end.Before(r.StartedAt) {
		return 0
	}
	return end.Sub(r.StartedAt)
}

// ListRecentRunsForPerson returns the person's most recent runs (any status),
// newest first, bounded by limit. Backs the doctor "recent runs" section.
func (s *Store) ListRecentRunsForPerson(ctx context.Context, tenantID, personID string, limit int) ([]RunDigest, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.task_id, COALESCE(t.title, ''), r.status, COALESCE(r.channel, ''),
		        COALESCE(r.last_error, ''), r.started_at, r.finished_at
		 FROM task_runs r LEFT JOIN tasks t ON t.id = r.task_id
		 WHERE r.tenant_id = ? AND r.person_id = ?
		 ORDER BY r.started_at DESC, r.id DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunDigest
	for rows.Next() {
		var d RunDigest
		var started int64
		var finished sql.NullInt64
		if err := rows.Scan(&d.RunID, &d.TaskID, &d.TaskTitle, &d.Status, &d.Channel, &d.LastError, &started, &finished); err != nil {
			return nil, err
		}
		d.StartedAt = time.Unix(started, 0)
		if finished.Valid && finished.Int64 > 0 {
			t := time.Unix(finished.Int64, 0)
			d.FinishedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ChannelMessageCount is a per-channel message tally for the doctor "activity by
// channel" section (the durable, person-scoped trajectory of where work happened).
type ChannelMessageCount struct {
	Channel string `json:"channel"`
	Count   int    `json:"count"`
}

// CountChannelMessagesByChannel returns the person's channel_messages tallied by
// channel, busiest first. Person-scoped and read-only.
func (s *Store) CountChannelMessagesByChannel(ctx context.Context, tenantID, personID string) ([]ChannelMessageCount, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, COUNT(1) FROM channel_messages
		 WHERE tenant_id = ? AND person_id = ?
		 GROUP BY channel ORDER BY COUNT(1) DESC, channel ASC`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelMessageCount
	for rows.Next() {
		var c ChannelMessageCount
		if err := rows.Scan(&c.Channel, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EventDigest is one recent task event for the doctor timeline: the real
// per-turn/tool/approval/error signal, which lives in task_events (control.db),
// not in the sparse gateway.log. Payload is a bounded preview; callers redact.
type EventDigest struct {
	TaskID    string    `json:"task_id"`
	TaskTitle string    `json:"task_title"`
	Type      string    `json:"type"`
	Channel   string    `json:"channel"`
	Preview   string    `json:"preview,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListRecentEventsForPerson returns the person's most recent task events across
// all their tasks (join through tasks for ownership + title), newest first.
// This is the doctor "recent events" timeline — the diagnostic detail that
// gateway.log does not carry.
func (s *Store) ListRecentEventsForPerson(ctx context.Context, tenantID, personID string, limit int) ([]EventDigest, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 40
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.task_id, COALESCE(t.title, ''), e.type, COALESCE(e.channel, ''),
		        COALESCE(e.payload_json, ''), e.created_at
		 FROM task_events e JOIN tasks t ON t.id = e.task_id
		 WHERE t.tenant_id = ? AND t.person_id = ?
		 ORDER BY e.created_at DESC, e.id DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventDigest
	for rows.Next() {
		var d EventDigest
		var payload string
		var created int64
		if err := rows.Scan(&d.TaskID, &d.TaskTitle, &d.Type, &d.Channel, &payload, &created); err != nil {
			return nil, err
		}
		payload = strings.ReplaceAll(strings.ReplaceAll(payload, "\n", " "), "\r", " ")
		if len(payload) > 160 {
			payload = payload[:160] + "…"
		}
		d.Preview = payload
		d.CreatedAt = time.Unix(created, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}
