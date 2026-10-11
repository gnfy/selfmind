package control

import (
	"context"
	"encoding/json"
	"fmt"
)

// RunRelevantExternalEffects selects unresolved effects of this exact lineage
// or named by its typed approval/observation records. It never scans prose or
// treats unrelated same-person work as this Run's result.
func (s *Store) RunRelevantExternalEffects(ctx context.Context, tenant, person, runID string) ([]ExternalEffectClaim, error) {
	tenant = normalizeTenant(tenant)
	run, err := s.GetRun(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.PersonID != person {
		return nil, fmt.Errorf("effect facts Run is unavailable for this person")
	}
	lineage, err := runLineage(ctx, s.db, tenant, person, runID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(lineage))
	for id := range lineage {
		ids = append(ids, id)
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	list := string(raw)
	claims, err := queryExternalEffectClaims(ctx, s.db, s.instance, `WHERE c.tenant_id=? AND c.person_id=? AND c.state<>'observed'
		AND (c.run_id IN (SELECT value FROM json_each(?))
		OR EXISTS (SELECT 1 FROM approval_requests a, json_each(a.payload_json,'$.external_effect_claim_ids') j
		  WHERE a.tenant_id=c.tenant_id AND a.person_id=c.person_id AND a.run_id IN (SELECT value FROM json_each(?)) AND j.value=c.id)
		OR EXISTS (SELECT 1 FROM task_events e WHERE e.run_id IN (SELECT value FROM json_each(?))
		  AND e.type=? AND json_extract(e.payload_json,'$.claim_id')=c.id)) ORDER BY c.created_at,c.id LIMIT 20`,
		tenant, person, list, list, list, ExternalEffectObservedEvent)
	for i := range claims {
		claims[i].SameChain = lineage[claims[i].RunID]
	}
	return claims, err
}
