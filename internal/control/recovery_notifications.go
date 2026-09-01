package control

import (
	"context"
	"encoding/json"
)

// RecoveryNotification is a durable daemon-recovery event that has not yet
// been surfaced through the person's active CLI or preferred IM endpoint.
type RecoveryNotification struct {
	EventID  string
	TenantID string
	PersonID string
	TaskID   string
	RunID    string
	Channel  string
	Title    string
}

// ListPendingRecoveryNotifications derives notification work from the event
// log. No second queue is needed: the companion run.recovery_notified event is
// the durable idempotency marker.
func (s *Store) ListPendingRecoveryNotifications(ctx context.Context, limit int) ([]RecoveryNotification, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, t.tenant_id, t.person_id, e.task_id, COALESCE(e.run_id, ''),
		        COALESCE(e.channel, ''), COALESCE(t.title, ''), COALESCE(e.payload_json, '')
		 FROM task_events e
		 JOIN tasks t ON t.id = e.task_id
		 WHERE e.type = 'run.interrupted'
		   AND NOT EXISTS (
		     SELECT 1 FROM task_events n
		      WHERE n.task_id = e.task_id AND COALESCE(n.run_id, '') = COALESCE(e.run_id, '')
		        AND n.type IN ('run.recovery_notified', 'run.recovery_scheduled')
		   )
		 ORDER BY e.cursor ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecoveryNotification
	for rows.Next() {
		var item RecoveryNotification
		var payload string
		if err := rows.Scan(&item.EventID, &item.TenantID, &item.PersonID, &item.TaskID, &item.RunID, &item.Channel, &item.Title, &payload); err != nil {
			return nil, err
		}
		var decoded struct {
			Outcome struct {
				CompletionReason string `json:"completion_reason"`
			} `json:"outcome"`
		}
		if json.Unmarshal([]byte(payload), &decoded) != nil || decoded.Outcome.CompletionReason != "daemon_recovery" {
			continue
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkAutomaticRecoveryScheduled is the durable marker that generic recovery
// claimed an interruption. It suppresses the fallback "reply continue"
// notification only after the idempotent queue row exists.
func (s *Store) MarkAutomaticRecoveryScheduled(ctx context.Context, item RecoveryNotification, mode, queueID string) error {
	payload, _ := json.Marshal(map[string]string{
		"recovery_event_id": item.EventID,
		"mode":              mode,
		"queue_id":          queueID,
	})
	_, err := s.AppendEvent(ctx, Event{
		TaskID: item.TaskID, RunID: item.RunID, Type: "run.recovery_scheduled", Visibility: "task", Channel: item.Channel,
		Payload: payload, IdempotencyKey: "run-recovery-scheduled:" + item.RunID,
	})
	return err
}

func (s *Store) MarkRecoveryNotificationSent(ctx context.Context, item RecoveryNotification) error {
	payload, _ := json.Marshal(map[string]string{"recovery_event_id": item.EventID})
	_, err := s.AppendEvent(ctx, Event{
		TaskID: item.TaskID, RunID: item.RunID, Type: "run.recovery_notified", Visibility: "task", Channel: item.Channel, Payload: payload,
	})
	return err
}
