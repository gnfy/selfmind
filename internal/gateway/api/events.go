package api

import (
	"encoding/json"
	"time"
)

const (
	EventDurable   = "durable"
	EventEphemeral = "ephemeral"
)

// RunEvent is the channel-neutral envelope emitted by the daemon event plane.
// Durable events carry Cursor and can be replayed with Last-Event-ID;
// ephemeral events (assistant deltas) carry only LiveSeq and are recovered by
// the synchronous final response when a subscriber falls behind.
type RunEvent struct {
	EventID    string          `json:"event_id,omitempty"`
	Cursor     int64           `json:"cursor,omitempty"`
	LiveSeq    uint64          `json:"live_seq,omitempty"`
	TenantID   string          `json:"tenant_id,omitempty"`
	PersonID   string          `json:"person_id,omitempty"`
	TaskID     string          `json:"task_id,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	Type       string          `json:"type"`
	Durability string          `json:"durability"`
	CreatedAt  time.Time       `json:"created_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}
