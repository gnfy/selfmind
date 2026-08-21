package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type GatewayRuntimeEvent struct {
	InstanceID string          `json:"instance_id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload_json,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (s *Store) RecordGatewayRuntimeEvent(ctx context.Context, event GatewayRuntimeEvent) (bool, error) {
	event.InstanceID = strings.TrimSpace(event.InstanceID)
	event.EventType = strings.TrimSpace(event.EventType)
	if event.InstanceID == "" || event.EventType == "" {
		return false, fmt.Errorf("gateway runtime event requires instance id and event type")
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO gateway_runtime_events
		(instance_id, event_type, payload_json, created_at) VALUES (?, ?, ?, ?)`,
		event.InstanceID, event.EventType, string(payload), createdAt.Unix())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *Store) LatestGatewayRuntimeEvent(ctx context.Context, eventType string) (*GatewayRuntimeEvent, error) {
	var event GatewayRuntimeEvent
	var payload string
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `SELECT instance_id, event_type, payload_json, created_at
		FROM gateway_runtime_events WHERE event_type = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		strings.TrimSpace(eventType)).Scan(&event.InstanceID, &event.EventType, &payload, &createdAt)
	if err != nil {
		return nil, err
	}
	event.Payload = json.RawMessage(payload)
	event.CreatedAt = time.Unix(createdAt, 0)
	return &event, nil
}

// GatewayRuntimeEventForInstance returns one lifecycle event for the exact
// daemon instance in the live status record. Callers must not infer current
// state from another instance's newer historical event.
func (s *Store) GatewayRuntimeEventForInstance(ctx context.Context, instanceID, eventType string) (*GatewayRuntimeEvent, error) {
	var event GatewayRuntimeEvent
	var payload string
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `SELECT instance_id, event_type, payload_json, created_at
		FROM gateway_runtime_events WHERE instance_id = ? AND event_type = ? LIMIT 1`,
		strings.TrimSpace(instanceID), strings.TrimSpace(eventType)).Scan(&event.InstanceID, &event.EventType, &payload, &createdAt)
	if err != nil {
		return nil, err
	}
	event.Payload = json.RawMessage(payload)
	event.CreatedAt = time.Unix(createdAt, 0)
	return &event, nil
}
