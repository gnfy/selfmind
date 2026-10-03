package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ExternalResourceWaitProjection shares the same blocker facts as admission.
// Readiness never releases claims or authorizes dispatch. NeedsObservation
// means no live producer can make progress while this Run is parked.
type ExternalResourceWaitProjection struct {
	ExternalResourceWait
	CreatedAt        time.Time
	RunStatus        string
	Ready            bool
	NeedsObservation bool
	BlockingClaims   []ExternalEffectClaim
}

type externalClaimReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func externalResourceBlockers(ctx context.Context, db externalClaimReader, tenantID, personID, waitingRun string, targets []string) ([]ExternalEffectClaim, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.id,c.tenant_id,c.person_id,c.run_id,c.effect_id,c.target_key,c.state,c.observation_ref,
		COALESCE(r.status,''), EXISTS (SELECT 1 FROM external_watches w
		 WHERE w.tenant_id=c.tenant_id AND w.person_id=c.person_id AND w.run_id=c.run_id
		 AND w.status IN ('pending','running','succeeded') AND COALESCE(w.finalized,0)=0
		 AND json_extract(w.preflight_receipt_json,'$.effect_id')=c.effect_id
		 AND COALESCE(json_extract(w.preflight_receipt_json,'$.effect_rule_key'),'') <> '')
		FROM external_effect_claims c LEFT JOIN runs r ON r.tenant_id=c.tenant_id AND r.id=c.run_id
		WHERE c.tenant_id=? AND c.person_id=? AND c.state <> 'observed' ORDER BY c.created_at,c.id`, tenantID, personID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var blockers []ExternalEffectClaim
	needsObservation := false
	for rows.Next() {
		var claim ExternalEffectClaim
		var status string
		var watch bool
		if err := rows.Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID, &claim.EffectID, &claim.TargetKey, &claim.State, &claim.ObservationRef, &status, &watch); err != nil {
			return nil, false, err
		}
		for _, target := range targets {
			if externalTargetsConflict(target, claim.TargetKey) {
				blockers = append(blockers, claim)
				if !watch && (status != "running" || claim.RunID == waitingRun) {
					needsObservation = true
				}
				break
			}
		}
	}
	return blockers, needsObservation, rows.Err()
}

// Empty owner filters are reserved for the daemon sweep. User-facing callers
// provide both owner fields. Claimed historical parents are excluded.
func (s *Store) ListExternalResourceWaits(ctx context.Context, tenantID, personID string, limit int) ([]ExternalResourceWaitProjection, error) {
	return s.listExternalResourceWaits(ctx, tenantID, personID, limit, true)
}

// Active waits have a separate bounded scan so historical blocked waits
// cannot starve new wakeups. Both scans use the same blocker projection.
func (s *Store) ListActiveExternalResourceWaits(ctx context.Context, limit int) ([]ExternalResourceWaitProjection, error) {
	return s.listExternalResourceWaits(ctx, "", "", limit, false)
}

func (s *Store) listExternalResourceWaits(ctx context.Context, tenantID, personID string, limit int, includeBlocked bool) ([]ExternalResourceWaitProjection, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("external resource store is unavailable")
	}
	if (tenantID == "") != (personID == "") {
		return nil, fmt.Errorf("resource wait requires both owner fields")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT w.tenant_id,w.person_id,w.run_id,w.effect_id,w.targets_json,w.status,w.created_at,r.status
		FROM external_resource_waits w JOIN runs r ON r.tenant_id=w.tenant_id AND r.id=w.run_id
		WHERE (w.status IN ('pending','queued') AND (r.status='waiting_external' OR (r.status='blocked' AND EXISTS (SELECT 1 FROM task_events e WHERE e.run_id=r.id AND e.type='run.finished' AND json_extract(e.payload_json,'$.resource_observation_required')=1)))) AND r.person_id=w.person_id
		AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.tenant_id=w.tenant_id AND child.resumes_run_id=w.run_id)`
	var args []any
	if !includeBlocked {
		query += ` AND r.status='waiting_external'`
	}
	if personID != "" {
		query += ` AND w.tenant_id=? AND w.person_id=?`
		args = append(args, normalizeTenant(tenantID), personID)
	}
	query += ` ORDER BY w.created_at,w.run_id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var waits []ExternalResourceWaitProjection
	for rows.Next() {
		var wait ExternalResourceWaitProjection
		var encoded string
		var created int64
		if err := rows.Scan(&wait.TenantID, &wait.PersonID, &wait.RunID, &wait.EffectID, &encoded, &wait.Status, &created, &wait.RunStatus); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &wait.TargetKeys); err != nil {
			_ = rows.Close()
			return nil, err
		}
		wait.CreatedAt = time.Unix(created, 0)
		waits = append(waits, wait)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range waits {
		wait := &waits[i]
		wait.BlockingClaims, wait.NeedsObservation, err = externalResourceBlockers(ctx, s.db, wait.TenantID, wait.PersonID, wait.RunID, wait.TargetKeys)
		if err != nil {
			return nil, err
		}
		wait.Ready = len(wait.BlockingClaims) == 0
	}
	return waits, nil
}

// Pending notices are derived from committed corrections, so a crash between
// commit and notification cannot silence an actionable wait.
func (s *Store) ListResourceObservationNotices(ctx context.Context) ([]RecoveryNotification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,t.tenant_id,t.person_id,e.thread_id,e.run_id,e.channel,t.title
		FROM task_events e JOIN threads t ON t.id=e.thread_id JOIN runs r ON r.id=e.run_id AND r.tenant_id=t.tenant_id
		WHERE e.type='run.finished' AND json_extract(e.payload_json,'$.resource_observation_required')=1
		AND r.status='blocked' AND NOT EXISTS (SELECT 1 FROM task_events n WHERE n.type='run.resource_observation_notified'
		 AND n.run_id=e.run_id AND json_extract(n.payload_json,'$.source_event_id')=e.id)
		ORDER BY e.cursor LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []RecoveryNotification
	for rows.Next() {
		var n RecoveryNotification
		if err := rows.Scan(&n.EventID, &n.TenantID, &n.PersonID, &n.TaskID, &n.RunID, &n.Channel, &n.Title); err != nil {
			return nil, err
		}
		notices = append(notices, n)
	}
	return notices, rows.Err()
}
