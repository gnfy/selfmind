package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Only a typed, exact checkpoint establishes parked human work. Historical
// waiting_user rows do not acquire this capability. A child that already owns
// the continuation must be cancelled by its own Run id.
const parkedHumanCondition = `r.status='waiting_user'
	AND COALESCE(r.resumed_by_run_id,'')=''
	AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.tenant_id=r.tenant_id AND child.resumes_run_id=r.id)
	AND c.outcome='await_human' AND COALESCE(c.wait_tool_call_id,'')<>''
	AND (
	  (c.wait_kind='approval' AND EXISTS (SELECT 1 FROM approval_requests a
	    WHERE a.tenant_id=r.tenant_id AND a.person_id=r.person_id AND a.run_id=r.id AND a.id=c.wait_ref
	      AND (a.status IN ('pending','archived') OR EXISTS (SELECT 1 FROM task_queue q
	        WHERE q.tenant_id=r.tenant_id AND q.person_id=r.person_id AND q.approval_id=a.id
	          AND q.status IN ('queued','started') AND COALESCE(q.run_id,'')=''))))
	  OR (c.wait_kind='clarification' AND EXISTS (SELECT 1 FROM clarify_requests a
	    WHERE a.tenant_id=r.tenant_id AND a.person_id=r.person_id AND a.run_id=r.id AND a.id=c.wait_ref
	      AND (a.status='pending' OR EXISTS (SELECT 1 FROM task_queue q
	        WHERE q.tenant_id=r.tenant_id AND q.person_id=r.person_id AND q.clarify_id=a.id
	          AND q.status IN ('queued','started') AND COALESCE(q.run_id,'')=''))))
	)`

const parkedHumanJoin = ` FROM runs r JOIN loop_checkpoints c
	ON c.tenant_id=r.tenant_id AND c.person_id=r.person_id AND c.run_id=r.id `

func (s *Store) ParkedHumanRunIDs(ctx context.Context, tenantID, personID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id`+parkedHumanJoin+`WHERE r.tenant_id=? AND r.person_id=? AND `+parkedHumanCondition,
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

// CancelParkedHumanRun terminates the human request and its unstarted recovery
// edge atomically with cancellation. A late answer can neither recreate a
// continuation nor use a one-shot authorization after the stop committed.
func (s *Store) CancelParkedHumanRun(ctx context.Context, tenantID, personID, runID string) (bool, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("person and run ids are required")
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	events, err := cancelParkedHumanTx(ctx, tx, tenantID, personID, runID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.publishParkedCancellation(events)
	return len(events) > 0, nil
}

func cancelParkedHumanTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID string) ([]Event, error) {
	var threadID, channel string
	err := tx.QueryRowContext(ctx, `SELECT r.thread_id,r.channel`+parkedHumanJoin+
		`WHERE r.tenant_id=? AND r.person_id=? AND r.id=? AND `+parkedHumanCondition,
		tenantID, personID, runID).Scan(&threadID, &channel)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resolutions, err := expireRunHumanRequestsTx(ctx, tx, tenantID, personID, runID, "user cancelled parked run")
	if err != nil {
		return nil, err
	}
	events, err := cancelParkedRunTx(ctx, tx, tenantID, personID, runID, threadID, channel, resolutions...)
	if err != nil {
		return nil, err
	}
	return events, nil
}

// A queue reference alone is not authority to close a Run. Resolve only the
// owned request bound to that Run's incomplete human checkpoint.
func queuedHumanParentTx(ctx context.Context, tx *sql.Tx, tenantID, personID, queueID string) (string, error) {
	var runID string
	err := tx.QueryRowContext(ctx, `SELECT r.id`+parkedHumanJoin+`
	  JOIN task_queue q ON q.tenant_id=r.tenant_id AND q.person_id=r.person_id
	    AND ((c.wait_kind='approval' AND q.approval_id=c.wait_ref)
	      OR (c.wait_kind='clarification' AND q.clarify_id=c.wait_ref))
	  WHERE q.tenant_id=? AND q.person_id=? AND q.id=? AND q.status IN ('queued','started')
	    AND COALESCE(q.run_id,'')='' AND `+parkedHumanCondition,
		tenantID, personID, queueID).Scan(&runID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return runID, err
}
