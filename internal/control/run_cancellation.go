package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrRunCancellationRequested refuses a stale result after the owner stopped
// its exact Run. The caller must materialize cancellation after its body exits.
var ErrRunCancellationRequested = errors.New("Run cancellation was requested")

// RequestRunCancel persists the stop before the gateway cancels its live handle.
// An exact uninvoked park may close immediately; other live bodies retain their
// Run and roots until observed exit. Both withdraw pending human authority.
func (s *Store) RequestRunCancel(ctx context.Context, tenantID, runID string) error {
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var personID, threadID, channel string
	var requested bool
	if err := tx.QueryRowContext(ctx, `SELECT person_id,thread_id,channel,COALESCE(cancel_requested,0) FROM runs WHERE tenant_id=? AND id=?`, tenantID, runID).Scan(&personID, &threadID, &channel, &requested); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	events, err := cancelParkedProviderTx(ctx, tx, tenantID, personID, runID)
	if err == nil && len(events) == 0 {
		events, err = cancelParkedHumanTx(ctx, tx, tenantID, personID, runID)
	}
	if err != nil {
		return err
	}
	if len(events) == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET cancel_requested=1 WHERE tenant_id=? AND id=?`, tenantID, runID); err != nil {
			return err
		}
		resolutions, err := expireRunHumanRequestsTx(ctx, tx, tenantID, personID, runID, "user requested stop")
		if err != nil {
			return err
		}
		if !requested {
			resolutions = append(resolutions, Event{Type: "run.cancel_requested", Payload: json.RawMessage(`{"reason":"user requested stop"}`), IdempotencyKey: "run:" + runID + ":cancel_requested"})
		}
		if err := cancelUnboundRunContinuationsTx(ctx, tx, tenantID, personID, runID); err != nil {
			return err
		}
		events, err = insertScopedEventsTx(ctx, tx, tenantID, personID, runID, threadID, channel, resolutions)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishParkedCancellation(events)
	return nil
}

func expireRunHumanRequestsTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID, reason string) ([]Event, error) {
	now := time.Now().Unix()
	rows, err := tx.QueryContext(ctx, `UPDATE approval_requests SET status='expired',updated_at=?
		WHERE tenant_id=? AND person_id=? AND run_id=? AND status='pending' RETURNING id`, now, tenantID, personID, runID)
	if err != nil {
		return nil, err
	}
	var resolutions []Event
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		data, _ := json.Marshal(map[string]string{"approval_id": id, "reason": reason})
		resolutions = append(resolutions, Event{Type: "approval.expired", Payload: data, IdempotencyKey: "approval:" + id + ":expired"})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approval_requests
		SET authorization_state='', authorization_expires_at=0, updated_at=?
		WHERE tenant_id=? AND person_id=? AND run_id=? AND COALESCE(authorization_state,'') <> 'consumed'`, now, tenantID, personID, runID); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `UPDATE clarify_requests SET status='expired',updated_at=?
		WHERE tenant_id=? AND person_id=? AND run_id=? AND status='pending' RETURNING id`, now, tenantID, personID, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		data, _ := json.Marshal(map[string]string{"clarify_id": id, "reason": reason})
		resolutions = append(resolutions, Event{Type: "clarify.expired", Payload: data, IdempotencyKey: "clarify:" + id + ":expired"})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return resolutions, nil
}

// Only unbound edges are withdrawn. A started child and unresolved external
// effect claims retain their own execution and observation authority.
func cancelUnboundRunContinuationsTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID string) error {
	if err := clearRunResumeSelectionsTx(ctx, tx, tenantID, personID, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_queue SET status='cancelled',claim_token='',lease_until=0
		WHERE tenant_id=? AND person_id=? AND (reply_to_run_id=? OR approval_id IN (SELECT id FROM approval_requests WHERE tenant_id=? AND person_id=? AND run_id=?) OR clarify_id IN (SELECT id FROM clarify_requests WHERE tenant_id=? AND person_id=? AND run_id=?)) AND status IN ('queued','started') AND COALESCE(run_id,'')=''`,
		tenantID, personID, runID, tenantID, personID, runID, tenantID, personID, runID); err != nil {
		return err
	}
	return nil
}

func insertScopedEventsTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID, threadID, channel string, events []Event) ([]Event, error) {
	now := time.Now()
	for i := range events {
		event := &events[i]
		event.ID, event.TenantID, event.PersonID = "event_"+uuid.NewString(), tenantID, personID
		event.TaskID, event.RunID, event.Channel = threadID, runID, channel
		event.Visibility, event.CreatedAt = "task", now
		if err := tx.QueryRowContext(ctx, `UPDATE event_sequence SET next_cursor=next_cursor+1 WHERE id=1 RETURNING next_cursor`).Scan(&event.Cursor); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_events (id,cursor,thread_id,run_id,type,visibility,channel,payload_json,idempotency_key,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, event.ID, event.Cursor, threadID, runID, event.Type, event.Visibility, channel, string(event.Payload), event.IdempotencyKey, now.Unix()); err != nil {
			return nil, err
		}
	}
	return events, nil
}
