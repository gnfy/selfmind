package tools

import (
	"fmt"
	"maps"

	"selfmind/internal/control"
)

// externalEffectForeignClaimsArg carries other Runs' left-uncertain claims
// that conflict with this call from the read-only precheck to the approval
// layer, so an ask the mode already makes can include the one-time decision
// about them.
const externalEffectForeignClaimsArg = "_external_effect_foreign_claims"

// externalEffectToleratedClaimsArg carries the claims a person's one-time
// approval of this exact call covered, from the approval layer to the claim.
const externalEffectToleratedClaimsArg = "_external_effect_tolerated_claims"

// ExternalEffectPrecheck prevents asking for authorization which cannot
// resolve an already-known unresolved effect of the Run's own chain. It never
// releases or reserves a claim; the claim gate decides again after approval.
func ExternalEffectPrecheck(store *control.Store) func(map[string]interface{}) error {
	return func(args map[string]interface{}) error {
		delete(args, externalEffectForeignClaimsArg)
		scope, installed := currentExecutionScopeAny(args)
		if !installed || !scope.ParallelWork {
			return nil
		}
		// A pending network capability ask must not hide a known blocker.
		// Project the requested route without granting or mutating it.
		projected := args
		if commandWantsNetwork(stringArg(args, "_tool_name"), args) {
			projected = maps.Clone(args)
			projected["_network_shared"] = true
		}
		if !externalEffectPossible(projected) {
			return nil
		}
		if store == nil {
			return fmt.Errorf("external effect admission store is unavailable; this call was not dispatched")
		}
		targets, _ := externalEffectTargets(projected, store)
		conflicts, err := store.InspectExternalEffectConflicts(contextFromArgs(args), scope.TenantID, scope.PersonID, scope.RunID, targets)
		if err != nil {
			return fmt.Errorf("external effect admission cannot be checked: %w", err)
		}
		if len(conflicts.Own) > 0 {
			return ownUnresolvedEffectError(scope.RunID, conflicts.All)
		}
		if len(conflicts.Foreign) > 0 {
			args[externalEffectForeignClaimsArg] = conflicts.Foreign
		}
		return nil
	}
}
