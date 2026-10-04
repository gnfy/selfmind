package tools

import (
	"fmt"
	"maps"
	"strings"

	"selfmind/internal/control"
)

// ExternalEffectPrecheck prevents asking for authorization which cannot
// resolve an already-known uncertain effect. It never releases a held claim.
func ExternalEffectPrecheck(store *control.Store) func(map[string]interface{}) error {
	return func(args map[string]interface{}) error {
		scope, installed := currentExecutionScopeAny(args)
		if !installed || !scope.ParallelWork {
			return nil
		}
		// A pending network capability ask must not hide a known blocker.
		// Project the requested route without granting or mutating it.
		if commandClearlyNeedsNetwork(stringArg(args, "_tool_name"), args) {
			args = maps.Clone(args)
			args["_network_shared"] = true
		}
		if !externalEffectPossible(args) {
			return nil
		}
		if store == nil {
			return fmt.Errorf("external effect admission store is unavailable; this call was not dispatched")
		}
		targets, _ := externalEffectTargets(args, store)
		claims, needsObservation, err := store.InspectExternalEffectBlockers(contextFromArgs(args), scope.TenantID, scope.PersonID, scope.RunID, targets)
		if err != nil {
			return fmt.Errorf("external effect admission cannot be checked: %w", err)
		}
		if !needsObservation {
			return nil
		}
		ids := make([]string, 0, len(claims))
		for _, claim := range claims {
			ids = append(ids, claim.ID)
		}
		message := fmt.Sprintf("External effect remains unresolved in run %s (claims: %s). This call was not dispatched. Human approval cannot establish the earlier effect's result.", claims[0].RunID, strings.Join(ids, ", "))
		return newStableToolRecoveryError(fmt.Errorf("%s", message), "external_effect_unresolved", "stale_precondition", message,
			"Inspect with a proven read-only command or an owner-approved observation script. Do not replay the earlier effect or clear its claim. If observation cannot establish the result, report the unresolved effect and the required human action.",
			"admission", "after_observation", "not_dispatched", false, "read_only_observation", "human_handoff")
	}
}
