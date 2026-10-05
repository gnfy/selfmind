package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ExternalEffectForObservation checks an intent to observe, not authority to
// release. A claim remains owned by its original Run and effect. The observer
// must be that Run or its exact, same-person continuation, never a Thread peer.
func (s *Store) ExternalEffectForObservation(ctx context.Context, tenantID, personID, observerRun, claimID string) (ExternalEffectClaim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalEffectClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return externalEffectForObservationTx(ctx, tx, normalizeTenant(tenantID), personID, observerRun, claimID)
}

func externalEffectForObservationTx(ctx context.Context, tx *sql.Tx, tenantID, personID, observerRun, claimID string) (ExternalEffectClaim, error) {
	var claim ExternalEffectClaim
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(observerRun) == "" || strings.TrimSpace(claimID) == "" {
		return claim, fmt.Errorf("exact person, observer Run and effect claim are required")
	}
	err := tx.QueryRowContext(ctx, `SELECT id,tenant_id,person_id,run_id,effect_id,target_key,state,observation_ref FROM external_effect_claims WHERE tenant_id=? AND person_id=? AND id=?`, tenantID, personID, claimID).Scan(&claim.ID, &claim.TenantID, &claim.PersonID, &claim.RunID, &claim.EffectID, &claim.TargetKey, &claim.State, &claim.ObservationRef)
	if err != nil {
		return claim, fmt.Errorf("effect claim not found for this person: %w", err)
	}
	var related int
	err = tx.QueryRowContext(ctx, `WITH RECURSIVE lineage(id,parent,depth) AS (
 SELECT id,COALESCE(resumes_run_id,''),0 FROM runs WHERE tenant_id=? AND person_id=? AND id=?
 UNION ALL SELECT r.id,COALESCE(r.resumes_run_id,''),l.depth+1 FROM runs r JOIN lineage l ON r.id=l.parent WHERE r.tenant_id=? AND r.person_id=? AND l.depth<?)
 SELECT COUNT(*) FROM lineage WHERE id=?`, tenantID, personID, observerRun, tenantID, personID, resumeChainMaxHops, claim.RunID).Scan(&related)
	if err != nil {
		return claim, err
	}
	if related != 1 {
		return claim, fmt.Errorf("observation must belong to the effect's exact Run or same-person continuation")
	}
	return claim, nil
}
