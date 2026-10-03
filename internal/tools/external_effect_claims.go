package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// ExternalEffectTargetProvider is a trusted built-in tool's proof of which
// external resources this exact invocation can change. Target keys are opaque
// non-secret identifiers, not URLs or credentials, and must cover every
// possible effect; completeOnReturn is true only when a successful
// tool return proves that the remote operation itself is finished. External
// tools and commands without such a proof use the person-wide unknown target.
type ExternalEffectTargetProvider interface {
	ExternalEffectTargets(args map[string]interface{}) (keys []string, completeOnReturn bool, err error)
}

// ExternalEffectClaimMiddleware sits immediately inside approval and before
// the tool body. Existing single-Run production behavior is unchanged; a Run
// admitted at parallel capacity must claim every external write target before
// dispatch. The claim survives a crash or unclear result until observation.
func ExternalEffectClaimMiddleware(store *control.Store) ResultMiddleware {
	return func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			scope, installed := currentExecutionScopeAny(args)
			if !installed || !scope.ParallelWork || !externalEffectPossible(args) {
				return next(args)
			}
			if store == nil || scope.TenantID == "" || scope.PersonID == "" || scope.RunID == "" {
				return uninvokedExternalEffect(fmt.Errorf("external effect claim cannot be established for this run"))
			}
			callID := strings.TrimSpace(stringArg(args, "_tool_call_id"))
			if callID == "" {
				return uninvokedExternalEffect(fmt.Errorf("external effect call has no durable identity"))
			}
			effectID := kernel.ToolEffectID(scope.RunID, callID)
			targets, completeOnReturn := externalEffectTargets(args, store)
			claim, err := store.ClaimExternalEffects(contextFromArgs(args), control.ExternalEffectClaimRequest{
				TenantID: scope.TenantID, PersonID: scope.PersonID, RunID: scope.RunID,
				EffectID: effectID, TargetKeys: targets,
			})
			if err != nil {
				return uninvokedExternalEffect(fmt.Errorf("external effect was not dispatched: %w", err))
			}
			if claim.AlreadyKnown {
				return uninvokedExternalEffect(fmt.Errorf("external effect %s was already claimed; inspect its observed state before retrying", callID))
			}
			if !claim.Granted {
				if claim.NeedsObservation {
					ids := make([]string, 0, len(claim.BlockingClaims))
					for _, held := range claim.BlockingClaims {
						ids = append(ids, held.ID)
					}
					message := fmt.Sprintf("External effect remains unresolved in run %s (claims: %s). This call was not dispatched. No automatic observation source can release this target.", claim.BlockedByRun, strings.Join(ids, ", "))
					return uninvokedExternalEffect(newStableToolRecoveryError(errors.New(message),
						"external_effect_unresolved", "stale_precondition", message,
						"Inspect with a proven read-only command or an owner-approved observation script. Do not replay the earlier effect or clear its claim. If observation cannot establish the result, report the unresolved effect and the required human action; do not promise automatic continuation.",
						"admission", "after_observation", "not_dispatched", false, "read_only_observation", "human_handoff"))
				}
				return uninvokedExternalEffect(externalResourcePause{blockedByRun: claim.BlockedByRun})
			}
			if err := store.MarkExternalEffectPossible(contextFromArgs(args), scope.TenantID, scope.RunID, effectID); err != nil {
				// The tool has not been called. A canceled request or failed
				// dispatch-boundary write must not strand a reserved target when
				// the store is still available for a separate cleanup write.
				cleanupCtx := context.WithoutCancel(contextFromArgs(args))
				_ = store.ObserveExternalEffectGroup(cleanupCtx, scope.TenantID, scope.PersonID,
					scope.RunID, effectID, "dispatch:not_started:"+callID)
				return uninvokedExternalEffect(fmt.Errorf("external effect could not cross the durable dispatch boundary: %w", err))
			}
			result, runErr := next(args)
			var observation string
			if result.Invoked != nil && !*result.Invoked || result.Process != nil && !result.Process.Started {
				observation = "dispatch:not_started:" + callID
			} else if runErr == nil && completeOnReturn {
				observation = "tool:completed:" + callID
			}
			if observation != "" {
				cleanupCtx := context.WithoutCancel(contextFromArgs(args))
				if err := store.ObserveExternalEffectGroup(cleanupCtx, scope.TenantID, scope.PersonID, scope.RunID, effectID, observation); err != nil {
					return result, errors.Join(runErr, fmt.Errorf("external effect observation was not saved: %w", err))
				}
			}
			return result, runErr
		}
	}
}

// The durable wait row was committed by ClaimExternalEffects before this
// boundary parks the Run. The daemon later resumes its exact parent; no tool
// call is replayed by the wakeup itself.
type externalResourcePause struct{ blockedByRun string }

func (p externalResourcePause) Error() string {
	return fmt.Sprintf("external target is occupied by run %s; this call was not dispatched", p.blockedByRun)
}
func (p externalResourcePause) ToolRunPause() (string, string, bool) {
	return "external_resource_wait", "This run is waiting for an occupied external target. The attempted tool call was not dispatched; SelfMind will resume this work after the target is observed and released.", false
}
func (externalResourcePause) ToolRunPauseStatus() string { return "waiting_external" }
func (externalResourcePause) ToolErrorCode() string      { return "external_resource_wait" }
func (externalResourcePause) ToolErrorCategory() string  { return "external_wait" }
func (p externalResourcePause) ModelSafeMessage() string { return p.Error() }
func (externalResourcePause) ToolRecoveryHint() string {
	return "The daemon will recheck the occupied target. A live owner or bound watcher must observe and release every conflicting claim before this exact work can continue."
}
func (externalResourcePause) ToolFailurePhase() string { return "admission" }
func (externalResourcePause) ToolRetryability() string { return "after_observation" }
func (externalResourcePause) ToolEffectState() string  { return "not_dispatched" }
func (externalResourcePause) ToolStateChanged() bool   { return false }

func uninvokedExternalEffect(err error) (kernel.ToolDispatchResult, error) {
	invoked := false
	return kernel.ToolDispatchResult{Invoked: &invoked}, err
}

func externalEffectPossible(args map[string]interface{}) bool {
	policy, known := args[toolExecutionPolicyArg].(toolExecutionPolicy)
	if !known || policy.Origin != ToolSchemaOriginBuiltin {
		return true
	}
	name := stringArg(args, "_tool_name")
	if isExecTool(name) {
		if name == "watch_external" {
			// Static proof restricts this built-in to one read-only check.
			return false
		}
		if observationOnlyExec(name, args) {
			// A deterministic read command or an exact owner-approved
			// observation script can inspect a held target without acquiring
			// another write claim. This is needed for evidence before release.
			return false
		}
		if effectiveSandboxModeArg(args) == SandboxHost {
			return true
		}
		sharedNetwork, _ := args["_network_shared"].(bool)
		credentialRead, _ := args[credentialReadArgKey].(bool)
		return sharedNetwork || credentialRead
	}
	if policy.ReadOnly {
		return false
	}
	if policy.Category == "network" {
		return true
	}
	for _, class := range policy.OperationClasses {
		if class == OpClassNetwork {
			return true
		}
	}
	return false
}

func externalEffectTargets(args map[string]interface{}, store *control.Store) ([]string, bool) {
	policy, known := args[toolExecutionPolicyArg].(toolExecutionPolicy)
	if !known || policy.Origin != ToolSchemaOriginBuiltin {
		return []string{control.UnknownExternalTarget}, false
	}
	if targets, ok := registeredEffectScriptTargets(args, store); ok {
		return targets, false
	}
	registry, ok := args["_registry"].(*Registry)
	if !ok || registry == nil {
		return []string{control.UnknownExternalTarget}, false
	}
	tool, ok := registry.Get(stringArg(args, "_tool_name"))
	if !ok {
		return []string{control.UnknownExternalTarget}, false
	}
	provider, ok := tool.(ExternalEffectTargetProvider)
	if !ok {
		return []string{control.UnknownExternalTarget}, false
	}
	keys, completeOnReturn, err := provider.ExternalEffectTargets(args)
	if err != nil || len(keys) == 0 {
		return []string{control.UnknownExternalTarget}, false
	}
	return keys, completeOnReturn
}
