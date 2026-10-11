package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The queue may already be leased, but once a child exists its execution
// owns cancellation. Human waits and live watchers keep their own controls.
const parkedProviderCondition = `r.status='waiting_external'
	AND COALESCE(r.resumed_by_run_id,'')=''
	AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.tenant_id=r.tenant_id AND child.resumes_run_id=r.id)
	AND NOT EXISTS (SELECT 1 FROM approval_requests a WHERE a.tenant_id=r.tenant_id AND a.run_id=r.id AND a.status='pending')
	AND NOT EXISTS (SELECT 1 FROM clarify_requests c WHERE c.tenant_id=r.tenant_id AND c.run_id=r.id AND c.status='pending')
	AND NOT EXISTS (SELECT 1 FROM external_watches w WHERE w.tenant_id=r.tenant_id AND w.run_id=r.id AND w.status IN ('pending','running'))
	AND EXISTS (SELECT 1 FROM task_queue q WHERE q.tenant_id=r.tenant_id AND q.person_id=r.person_id
	  AND q.reply_to_run_id=r.id AND (q.idempotency_key IN ('provider-wait:'||r.id,'execution-lane:'||r.id)
	    OR (` + resourceWakeQueueCondition + `))
	  AND q.status IN ('queued','started') AND COALESCE(q.run_id,'')='')`

// ParkedProviderRunIDs lists exact cancellable automatic waits, including ones
// not shown as resumable Attention because their automatic wakeup owns them.
func (s *Store) ParkedProviderRunIDs(ctx context.Context, tenantID, personID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id FROM runs r WHERE r.tenant_id=? AND r.person_id=? AND `+parkedProviderCondition,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CancelParkedProviderRun atomically closes the parked Run and every unstarted
// continuation of it. A launcher either creates its child first or loses its
// queue claim and parent claim; a stopped Run cannot return on the next drain.
func (s *Store) CancelParkedProviderRun(ctx context.Context, tenantID, personID, runID string) (bool, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("person and run ids are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	events, err := cancelParkedProviderTx(ctx, tx, normalizeTenant(tenantID), personID, runID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.publishParkedCancellation(events)
	return len(events) > 0, nil
}

func cancelParkedProviderTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID string) ([]Event, error) {
	var threadID, channel string
	err := tx.QueryRowContext(ctx, `SELECT r.thread_id,r.channel FROM runs r WHERE r.tenant_id=? AND r.person_id=? AND r.id=? AND `+parkedProviderCondition,
		tenantID, personID, runID).Scan(&threadID, &channel)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cancelParkedRunTx(ctx, tx, tenantID, personID, runID, threadID, channel)
}

// The caller proves an unclaimed parked Run in the same transaction. Both
// provider and human waits then close the Run, pending queue and event ledger
// together; an event failure cannot leave half-cancelled work.
func cancelParkedRunTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID, threadID, channel string, resolutions ...Event) ([]Event, error) {
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='cancelled',cancel_requested=1,finished_at=?,heartbeat_at=? WHERE tenant_id=? AND person_id=? AND id=?`,
		now.Unix(), now.Unix(), tenantID, personID, runID); err != nil {
		return nil, err
	}
	if err := cancelUnboundRunContinuationsTx(ctx, tx, tenantID, personID, runID); err != nil {
		return nil, err
	}
	outcome := map[string]interface{}{"status": "cancelled", "completion_reason": "cancelled", "resumable": false, "summary": "The parked run was cancelled before continuation."}
	events := resolutions
	for _, eventType := range []string{"run.outcome", "run.cancelled"} {
		payload := outcome
		if eventType == "run.cancelled" {
			payload = map[string]interface{}{"outcome": outcome}
		}
		encoded, _ := json.Marshal(payload)
		events = append(events, Event{Type: eventType, Payload: encoded, IdempotencyKey: "run:" + runID + ":cancelled:" + eventType})
	}
	return insertScopedEventsTx(ctx, tx, tenantID, personID, runID, threadID, channel, events)
}

func (s *Store) publishParkedCancellation(events []Event) {
	if s.events != nil {
		for _, event := range events {
			s.events.publish(event)
		}
	}
}

// CancelQueued drops one still-unstarted item and closes the exact parked wait
// whose wakeup it owned, in the same transaction. A bound child is live work.
func (s *Store) CancelQueued(ctx context.Context, tenantID, personID, queueID string) (bool, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(queueID) == "" {
		return false, fmt.Errorf("person and queue ids are required")
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var parent, key, status string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(reply_to_run_id,''),COALESCE(idempotency_key,''),status FROM task_queue
		WHERE tenant_id=? AND person_id=? AND id=? AND COALESCE(run_id,'')=''
		AND status IN ('queued','started')`, tenantID, personID, queueID).Scan(&parent, &key, &status)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	humanParent, err := queuedHumanParentTx(ctx, tx, tenantID, personID, queueID)
	if err != nil {
		return false, err
	}
	resourceParent, err := queuedResourceWaitParentTx(ctx, tx, tenantID, personID, queueID)
	if err != nil {
		return false, err
	}
	providerWait := parent != "" && (key == "provider-wait:"+parent || key == "execution-lane:"+parent || resourceParent == parent)
	if status == QueueStatusStarted && !providerWait && humanParent == "" {
		return false, nil
	}
	var events []Event
	if providerWait {
		events, err = cancelParkedProviderTx(ctx, tx, tenantID, personID, parent)
		if err != nil {
			return false, err
		}
	}
	if humanParent != "" {
		cancelled, err := cancelParkedHumanTx(ctx, tx, tenantID, personID, humanParent)
		if err != nil {
			return false, err
		}
		events = append(events, cancelled...)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_queue SET status='cancelled',claim_token='',lease_until=0 WHERE tenant_id=? AND person_id=? AND id=?`, tenantID, personID, queueID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.publishParkedCancellation(events)
	return true, nil
}

func (s *Store) clearQueuedAndParkedRuns(ctx context.Context, tenantID, personID string) (int, error) {
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(reply_to_run_id,''),COALESCE(idempotency_key,''),status FROM task_queue
		WHERE tenant_id=? AND person_id=? AND COALESCE(run_id,'')='' AND status IN ('queued','started')`, tenantID, personID)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, parent, key, status string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.parent, &item.key, &item.status); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var ids []any
	parents, humanParents := map[string]bool{}, map[string]bool{}
	for _, item := range candidates {
		humanParent, err := queuedHumanParentTx(ctx, tx, tenantID, personID, item.id)
		if err != nil {
			return 0, err
		}
		resourceParent, err := queuedResourceWaitParentTx(ctx, tx, tenantID, personID, item.id)
		if err != nil {
			return 0, err
		}
		providerWait := item.parent != "" && (item.key == "provider-wait:"+item.parent || item.key == "execution-lane:"+item.parent || resourceParent == item.parent)
		if item.status == QueueStatusStarted && !providerWait && humanParent == "" {
			continue
		}
		ids = append(ids, item.id)
		if providerWait {
			parents[item.parent] = true
		}
		if humanParent != "" {
			humanParents[humanParent] = true
		}
	}
	var events []Event
	for parent := range parents {
		cancelled, err := cancelParkedProviderTx(ctx, tx, tenantID, personID, parent)
		if err != nil {
			return 0, err
		}
		events = append(events, cancelled...)
	}
	for parent := range humanParents {
		cancelled, err := cancelParkedHumanTx(ctx, tx, tenantID, personID, parent)
		if err != nil {
			return 0, err
		}
		events = append(events, cancelled...)
	}
	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE task_queue SET status='cancelled',claim_token='',lease_until=0 WHERE id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.publishParkedCancellation(events)
	return len(ids), nil
}
