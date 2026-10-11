package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ExternalResourceWaitProjection shares the same blocker facts as admission.
// Readiness never releases claims or authorizes dispatch. NeedsObservation
// means nothing the Run waits for is left and a decision or an observation
// is needed before it can continue.
type ExternalResourceWaitProjection struct {
	ExternalResourceWait
	CreatedAt        time.Time
	RunStatus        string
	Ready            bool
	NeedsObservation bool
	BlockingClaims   []ExternalEffectClaim
	// WakeOnRelease: the blocked Run's correction promised an automatic
	// continuation once its claims are released.
	WakeOnRelease bool
}

// InspectExternalEffectBlockers is advisory and read-only. ClaimExternalEffects
// remains the atomic dispatch authority, including changes during an approval.
// The flag reports that the call cannot proceed by waiting: the Run's own
// unresolved effect, or, once nothing is waited for, another Run's
// left-uncertain effect.
func (s *Store) InspectExternalEffectBlockers(ctx context.Context, tenant, person, runID string, targets []string) ([]ExternalEffectClaim, bool, error) {
	conflicts, err := s.InspectExternalEffectConflicts(ctx, tenant, person, runID, targets)
	if err != nil {
		return nil, false, err
	}
	return conflicts.All, conflicts.needsDecision(), nil
}

// InspectExternalEffectConflicts classifies, read-only, the claims that
// conflict with a target set as the running Run would meet them at its claim.
func (s *Store) InspectExternalEffectConflicts(ctx context.Context, tenant, person, runID string, targets []string) (ExternalEffectConflicts, error) {
	run, err := s.GetRun(ctx, tenant, runID)
	if err != nil {
		return ExternalEffectConflicts{}, err
	}
	if run == nil || person == "" || run.PersonID != person || run.Status != "running" {
		return ExternalEffectConflicts{}, fmt.Errorf("external effect run is not active for this person")
	}
	return classifyExternalBlockers(ctx, s.db, s.instance, normalizeTenant(tenant), person, runID, targets)
}

// externalResourceBlockers is the parked-wait view. The flag reports that
// nothing is left to wait for and a decision or an observation is needed, so
// the parked Run cannot be promised an automatic continuation; while a claim
// is still waited for, the Run stays parked, as the claim gate orders it.
func externalResourceBlockers(ctx context.Context, db externalClaimQuerier, instance, tenantID, personID, waitingRun string, targets []string) ([]ExternalEffectClaim, bool, error) {
	conflicts, err := classifyExternalBlockers(ctx, db, instance, tenantID, personID, waitingRun, targets)
	if err != nil {
		return nil, false, err
	}
	return conflicts.All, conflicts.needsDecision(), nil
}

// ResourceWakeOnReleasePayloadKey marks a resource-observation correction
// written by a daemon that wakes the blocked Run once its claims are
// released. A correction without it was told to /resume and stays with the
// person.
const ResourceWakeOnReleasePayloadKey = "resource_wake_on_release"

// ListWakeableBlockedResourceWaits returns Runs blocked on an unresolved
// effect whose correction promised a wakeup and whose whole target set is now
// free. Readiness is advisory: the continuation's next effect claims again.
// The scan is bounded and newest first, so old waits that stay blocked cannot
// hold back a recent release.
func (s *Store) ListWakeableBlockedResourceWaits(ctx context.Context, limit int) ([]ExternalResourceWait, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("external resource store is unavailable")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT w.tenant_id,w.person_id,w.run_id,w.effect_id,w.targets_json,w.status
		FROM external_resource_waits w JOIN runs r ON r.tenant_id=w.tenant_id AND r.id=w.run_id AND r.person_id=w.person_id
		WHERE w.status IN ('pending','queued') AND r.status='blocked'
		AND EXISTS (SELECT 1 FROM task_events e WHERE e.run_id=r.id AND e.type='run.finished'
			AND json_extract(e.payload_json,'$.resource_observation_required')=1
			AND json_extract(e.payload_json,'$.`+ResourceWakeOnReleasePayloadKey+`')=1)
		AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.tenant_id=w.tenant_id AND child.resumes_run_id=w.run_id)
		ORDER BY w.created_at DESC, w.run_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var waits []ExternalResourceWait
	for rows.Next() {
		var wait ExternalResourceWait
		var encoded string
		if err := rows.Scan(&wait.TenantID, &wait.PersonID, &wait.RunID, &wait.EffectID, &encoded, &wait.Status); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &wait.TargetKeys); err != nil {
			_ = rows.Close()
			return nil, err
		}
		waits = append(waits, wait)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var ready []ExternalResourceWait
	for _, wait := range waits {
		blockers, _, err := externalResourceBlockers(ctx, s.db, s.instance, wait.TenantID, wait.PersonID, wait.RunID, wait.TargetKeys)
		if err != nil {
			return nil, err
		}
		if len(blockers) == 0 {
			ready = append(ready, wait)
		}
	}
	return ready, nil
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
	query := `SELECT w.tenant_id,w.person_id,w.run_id,w.effect_id,w.targets_json,w.status,w.created_at,r.status,
		EXISTS (SELECT 1 FROM task_events e WHERE e.run_id=r.id AND e.type='run.finished'
			AND json_extract(e.payload_json,'$.` + ResourceWakeOnReleasePayloadKey + `')=1)
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
		if err := rows.Scan(&wait.TenantID, &wait.PersonID, &wait.RunID, &wait.EffectID, &encoded, &wait.Status, &created, &wait.RunStatus, &wait.WakeOnRelease); err != nil {
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
		wait.BlockingClaims, wait.NeedsObservation, err = externalResourceBlockers(ctx, s.db, s.instance, wait.TenantID, wait.PersonID, wait.RunID, wait.TargetKeys)
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
