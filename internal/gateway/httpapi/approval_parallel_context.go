package httpapi

import (
	"context"
	"fmt"
	"selfmind/internal/control"
	"selfmind/internal/tools"
	"sort"
	"strings"
)

func (c *RunCoordinator) parallelExecutionContext(ctx context.Context, identity *control.IdentityContext, runID string) ([]tools.ParallelExecutionContext, error) {
	facts := map[string]*tools.ParallelExecutionContext{}
	if c == nil || c.srv == nil || c.srv.Control == nil {
		return nil, fmt.Errorf("parallel execution state is unavailable")
	}
	for _, active := range c.activeRunsForPerson(identity.PersonID) {
		if active.TenantID != identity.TenantID || active.RunID == "" || active.RunID == runID {
			continue
		}
		fact := &tools.ParallelExecutionContext{RunID: active.RunID}
		for _, root := range active.ExecutionRoots {
			if strings.TrimSpace(root.Path) != "" {
				fact.Directories = append(fact.Directories, root.Path)
			}
		}
		facts[active.RunID] = fact
	}
	claims, err := c.srv.Control.ListUnresolvedExternalEffects(ctx, identity.TenantID, identity.PersonID, 32)
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		if claim.RunID == runID || (claim.Disposition != control.ExternalClaimInFlight && claim.Disposition != control.ExternalClaimWatched) {
			continue
		}
		fact := facts[claim.RunID]
		if fact == nil {
			fact = &tools.ParallelExecutionContext{RunID: claim.RunID}
			facts[claim.RunID] = fact
		}
		fact.InFlightTargets = append(fact.InFlightTargets, claim.TargetKey)
	}
	keys := make([]string, 0, len(facts))
	for key := range facts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]tools.ParallelExecutionContext, 0, len(keys))
	for _, key := range keys {
		out = append(out, *facts[key])
	}
	return out, nil
}
