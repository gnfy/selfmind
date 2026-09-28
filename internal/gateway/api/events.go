package api

import (
	"encoding/json"
	"time"
)

const (
	EventDurable   = "durable"
	EventEphemeral = "ephemeral"
)

// RunEvent is the envelope emitted by the daemon event plane. Durable events
// carry Cursor and can be replayed with Last-Event-ID; ephemeral events
// (assistant deltas) carry only LiveSeq and are recovered by the synchronous
// final response when a subscriber falls behind.
//
// Channel is the session the event concerns: a run's text, tools, plan and
// usage belong to the session that started it and reach only that session and
// clients attached to the run. Empty means the person as a whole.
type RunEvent struct {
	EventID    string          `json:"event_id,omitempty"`
	Cursor     int64           `json:"cursor,omitempty"`
	LiveSeq    uint64          `json:"live_seq,omitempty"`
	TenantID   string          `json:"tenant_id,omitempty"`
	PersonID   string          `json:"person_id,omitempty"`
	TaskID     string          `json:"task_id,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	Channel    string          `json:"channel,omitempty"`
	Type       string          `json:"type"`
	Durability string          `json:"durability"`
	CreatedAt  time.Time       `json:"created_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}
