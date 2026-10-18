package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// RunCompletionSink is the coordinator-owned atomic completion seam. A false
// handled result preserves legacy/deferred selection flows; a Plan patch may
// never silently fall back to a non-atomic write.
type RunCompletionSink interface {
	CompleteRun(context.Context, map[string]interface{}, []PlanStep) (handled bool, err error)
}

type runCompletionSinkKey struct{}

func WithRunCompletionSink(ctx context.Context, sink RunCompletionSink) context.Context {
	return context.WithValue(ctx, runCompletionSinkKey{}, sink)
}

func prepareAtomicFinish(args map[string]interface{}, out map[string]interface{}) (bool, error) {
	raw, supplied := args["plan_patch"]
	sink, _ := ContextFromArgs(args).Value(runCompletionSinkKey{}).(RunCompletionSink)
	// The final body uses the existing atomic finalizer even without a Plan.
	// Legacy summary-only calls and standalone/delegated adapters retain their
	// released path. A patch can never silently acquire a non-atomic fallback.
	if !supplied && (sink == nil || out["final_response"] == nil || out["status"] == "running" || legacyFinishPayload(args)) {
		return false, nil
	}
	if out["status"] == "running" {
		return false, finishInputError(fmt.Errorf("an atomic final Plan submission needs a stopping outcome"), "tool_arguments_invalid", "Continue working, or choose the appropriate completed or waiting outcome.")
	}
	var patch []PlanStep
	if supplied {
		var err error
		patch, err = planStepsFromArgs(raw)
		if err != nil || len(patch) == 0 {
			return false, finishInputError(fmt.Errorf("plan_patch needs changed current steps with step_id and status"), "tool_arguments_invalid", "Supply only final changes to current Plan steps.")
		}
	}
	if out["final_response"] == nil {
		return false, finishInputError(fmt.Errorf("final_response is required with plan_patch"), "tool_arguments_invalid", "Write the complete final reply in final_response and submit the final changes together.")
	}
	if sink == nil {
		return false, toolAvailabilityPreparationError(fmt.Errorf("durable completion is unavailable in this execution context"), "completion_unavailable")
	}
	if raw, exists := args["assessments"]; exists {
		// The canonical validator runs against the candidate Plan at the sink.
		// Keep the original proposal here; do not pre-validate against old state.
		encoded, err := json.Marshal(raw)
		if err != nil {
			return false, err
		}
		var assessments []interface{}
		if json.Unmarshal(encoded, &assessments) != nil || len(assessments) == 0 || len(assessments) > 16 {
			return false, finishInputError(fmt.Errorf("assessments must contain 1–16 condition judgments"), "condition_assessment_invalid", "Correct the condition findings and resend finish_run.")
		}
		out["assessments"] = raw
	}
	handled, err := sink.CompleteRun(ContextFromArgs(args), out, patch)
	if !handled && err == nil && supplied {
		return false, finishInputError(fmt.Errorf("this pending handoff must resolve before atomic Plan completion"), "completion_precondition", "Preserve the pending work and finish its selection or handoff first.")
	}
	return handled, err
}

// Released clients may still submit the old duplicated bookkeeping fields.
// Preserve their coordinator contract; only the advertised compact payload (or
// an explicit atomic patch) opts into candidate-state completion.
func legacyFinishPayload(args map[string]interface{}) bool {
	for _, name := range []string{"summary", "done", "files", "tests", "need_approve"} {
		if _, supplied := args[name]; supplied {
			return true
		}
	}
	return false
}

func finalPlanPatchProperty() PropertyDef {
	// Finalization changes progress, not goals. Do not repeat the planning
	// schema (and its criteria/scope editing fields) in every model request.
	return PropertyDef{Type: "array", Description: "Final changed steps; requires final_response. Omission preserves state. Goals/criteria stay fixed; cancellation and reuse follow update_plan rules.", Items: &PropertyDef{Type: "object", AdditionalProperties: rejectAdditionalProperties(), Required: []string{"step_id", "status"}, Properties: map[string]PropertyDef{
		"step_id":                  {Type: "string", Description: "Current server-issued step id."},
		"status":                   {Type: "string", Enum: []string{"pending", "in_progress", "completed", "cancelled"}},
		"reuse_prior_verification": {Type: "boolean"},
		"reuse_reason":             {Type: "string"},
		"cancellation_disposition": {Type: "string", Enum: []string{"not_required", "user_takeover", "unfinished"}},
		"cancellation_reason":      {Type: "string"},
		"user_takeover_quote":      {Type: "string"},
		"replacement_step_id":      {Type: "string"},
		"scope_change_quote":       {Type: "string"},
	}}}
}
