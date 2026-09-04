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
		`SELECT r.id, r.thread_id, COALESCE(t.title, ''), r.status, COALESCE(r.channel, ''),
		        COALESCE(r.last_error, ''), r.started_at, r.finished_at
		 FROM runs r LEFT JOIN threads t ON t.id = r.thread_id
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

// ErrorEntry is one recent failure for the doctor "recent errors" section: a
// run-level failure (model/interface error, interrupted run) or a tool-failure
// event (a tool.completed carrying an error). Aggregating both into one
// newest-first list answers "what has been going wrong lately" without reading
// per-run event logs by hand.
type ErrorEntry struct {
	When    time.Time `json:"when"`
	Kind    string    `json:"kind"`   // "run" | "tool"
	Source  string    `json:"source"` // run status, or the failing tool name
	Message string    `json:"message"`
	TaskID  string    `json:"task_id,omitempty"`
}

// ListRecentErrors returns the person's most recent failures, newest first,
// read entirely from the durable task_events log (the authoritative,
// per-event, immutable record — unlike task_runs.last_error, which ordinary
// model failures never set):
//   - tool.completed events carrying a non-empty "error" (tool failures,
//     Kind "tool", Source = tool name);
//   - run.finished events whose outcome.status is a failure (run/model
//     failures, Kind "run", Source = the failed status, Message = the
//     outcome summary — where a 429/EOF surfaces);
//   - run.finalize_error events (Kind "run", finalization defects).
//
// Read-only, person-scoped (via the task join), bounded. The caller redacts
// secrets before display.
func (s *Store) ListRecentErrors(ctx context.Context, tenantID, personID string, limit int) ([]ErrorEntry, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.type, e.created_at, e.thread_id,
		        COALESCE(json_extract(e.payload_json,'$.tool'),''),
		        COALESCE(json_extract(e.payload_json,'$.error'),''),
		        COALESCE(json_extract(e.payload_json,'$.outcome.status'),''),
		        COALESCE(json_extract(e.payload_json,'$.outcome.summary'),''),
		        COALESCE(json_extract(e.payload_json,'$.errors'),'')
		 FROM task_events e JOIN threads t ON t.id = e.thread_id
		 WHERE t.tenant_id = ? AND t.person_id = ?
		   AND (
		     (e.type = 'tool.completed' AND TRIM(COALESCE(json_extract(e.payload_json,'$.error'),'')) <> '')
		     OR (e.type = 'run.finished' AND LOWER(COALESCE(json_extract(e.payload_json,'$.outcome.status'),'')) IN ('failed','error'))
		     OR e.type = 'run.finalize_error'
		   )
		 ORDER BY e.created_at DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ErrorEntry
	for rows.Next() {
		var typ, taskID, tool, toolErr, outStatus, outSummary, finalizeErrs string
		var ts int64
		if err := rows.Scan(&typ, &ts, &taskID, &tool, &toolErr, &outStatus, &outSummary, &finalizeErrs); err != nil {
			return nil, err
		}
		e := ErrorEntry{When: time.Unix(ts, 0), TaskID: taskID}
		switch typ {
		case "tool.completed":
			e.Kind = "tool"
			e.Source = tool
			if e.Source == "" {
				e.Source = "tool"
			}
			e.Message = toolErr
		case "run.finished":
			e.Kind = "run"
			e.Source = outStatus
			e.Message = outSummary
		case "run.finalize_error":
			e.Kind = "run"
			e.Source = "finalize_error"
			e.Message = finalizeErrs
		}
		out = append(out, e)
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
		`SELECT e.thread_id, COALESCE(t.title, ''), e.type, COALESCE(e.channel, ''),
		        COALESCE(e.payload_json, ''), e.created_at
		 FROM task_events e JOIN threads t ON t.id = e.thread_id
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
