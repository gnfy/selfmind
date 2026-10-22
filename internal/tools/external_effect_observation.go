package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

const (
	effectObservationReadsNoState = "effect_claim_id needs an observation that reads the affected target's state; this command reads nothing beyond its own arguments, the environment, or the clock. This call was not dispatched."
	// The hint names what counts without choosing the target for the model.
	effectObservationReadsNoStateHint = "Observe the target itself, such as its remote, service, or the files the effect changed, with a read-only command, then bind that check."
)

// observeBoundExternalEffect runs a read-only observation the model bound to
// an effect claim with effect_claim_id. The model judges whether the check
// shows the effect's outcome; the runtime checks that the command is a proven
// read-only observation, that the claim is the person's and not in flight,
// that the observation starts after the effect, and that it succeeds. A
// success releases a claim of this Run's own chain, or lets this Run pass
// another Run's claim once without settling it.
func observeBoundExternalEffect(store *control.Store, scope ExecutionScope, installed bool, args map[string]interface{},
	claimID string, next ResultExecutor) (kernel.ToolDispatchResult, error) {
	name := stringArg(args, "_tool_name")
	refuse := func(code, message, hint string) (kernel.ToolDispatchResult, error) {
		return uninvokedExternalEffect(newStableToolRecoveryError(errors.New(message), code, "invalid_request", message, hint,
			"admission", "corrected_input", "not_dispatched", false))
	}
	if !observationOnlyExec(name, args) {
		return refuse("effect_observation_not_read_only",
			"effect_claim_id binds only a proven read-only observation: a catalogued read-only command with literal arguments, or an owner-approved observation script. This call was not dispatched.",
			"Run a read-only check that shows the effect's result, or omit effect_claim_id for other commands.")
	}
	if !observationReadsState(name, args) {
		return refuse("effect_observation_reads_no_state", effectObservationReadsNoState, effectObservationReadsNoStateHint)
	}
	if store == nil || !installed || scope.TenantID == "" || scope.PersonID == "" || scope.RunID == "" {
		return refuse("effect_observation_unavailable", "This Run cannot bind an observation to an effect claim. This call was not dispatched.",
			"Report the unresolved effect; the person can settle it with /effects settle.")
	}
	callID := strings.TrimSpace(stringArg(args, "_tool_call_id"))
	if callID == "" {
		return refuse("effect_observation_unavailable", "The observation has no durable call identity. This call was not dispatched.",
			"Retry the observation as a normal tool call.")
	}
	ctx := contextFromArgs(args)
	claim, err := store.ExternalEffectObservationTarget(ctx, scope.TenantID, scope.PersonID, scope.RunID, claimID)
	switch {
	case errors.Is(err, control.ErrExternalEffectClaimNotFound):
		return refuse("effect_claim_not_found", fmt.Sprintf("No unresolved external effect claim %s belongs to this person. This call was not dispatched.", claimID),
			"Use the exact claim id from the refusal or failure that named it.")
	case errors.Is(err, control.ErrExternalEffectAlreadyReleased):
		return refuse("effect_claim_released", fmt.Sprintf("External effect claim %s is already released; no observation is needed. This call was not dispatched.", claimID),
			"Continue without binding an observation to this claim.")
	case errors.Is(err, control.ErrExternalEffectInFlight):
		return refuse("effect_claim_in_flight", fmt.Sprintf("External effect claim %s is still in flight: its call has not returned, so an observation now could precede its result. This call was not dispatched.", claimID),
			"Wait for the call to return; a successful return settles the claim by itself.")
	case err != nil:
		return uninvokedExternalEffect(fmt.Errorf("effect observation was not dispatched: %w", err))
	}
	start, err := store.AllocateEffectSequence(ctx)
	if err != nil {
		return uninvokedExternalEffect(fmt.Errorf("effect observation was not dispatched: %w", err))
	}
	result, runErr := next(args)
	if runErr != nil || !callProvedSuccess(name, result) {
		note := fmt.Sprintf("This observation did not succeed, so external effect claim %s stays unresolved.", claim.ID)
		if runErr != nil {
			return result, &externalEffectNoteError{cause: runErr, note: note}
		}
		result.Output = strings.TrimRight(result.Output, "\n") + "\n\n" + note
		return result, nil
	}
	bound, err := store.BindExternalEffectObservation(context.WithoutCancel(ctx), control.ExternalEffectObservation{
		TenantID: scope.TenantID, PersonID: scope.PersonID, ObserverRunID: scope.RunID,
		ClaimID: claim.ID, ToolCallID: callID, StartSequence: start,
	})
	var note string
	switch {
	case err != nil:
		note = fmt.Sprintf("The observation succeeded but was not bound to external effect claim %s: %v. The claim is unchanged.", claim.ID, err)
	case bound.Released:
		note = fmt.Sprintf("Bound this successful observation to external effect claim %s and released it (observation %s). Effects of this work may continue.",
			claim.ID, bound.Claim.ObservationRef)
	default:
		note = fmt.Sprintf("Bound this successful observation to external effect claim %s of run %s. That claim stays unresolved for its own run; "+
			"this run's next call on a conflicting target may pass it once without asking the person.", claim.ID, claim.RunID)
	}
	result.Output = strings.TrimRight(result.Output, "\n") + "\n\n" + note
	return result, nil
}
