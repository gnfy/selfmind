package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type FinishRunTool struct {
	BaseTool
	store        *PlanStore
	legacySchema ToolSchema
}

var finishRunStatuses = []string{
	"done", "blocked", "failed", "running", "waiting_external", "waiting_user", "needs_approval",
}

func validFinishRunStatus(status string) bool {
	for _, allowed := range finishRunStatuses {
		if status == allowed {
			return true
		}
	}
	return false
}

func NewFinishRunTool() *FinishRunTool {
	return NewFinishRunToolWithStore(nil)
}

func NewFinishRunToolWithStore(store *PlanStore) *FinishRunTool {
	tool := &FinishRunTool{
		BaseTool: BaseTool{
			name:        "finish_run",
			description: "Finish after observing work results. Submit status and final_response once; include final changed steps in plan_patch instead of a separate final update_plan. Omit plan_patch if no steps change. assessments are Main's condition judgments; step_id with evidence_refs can associate existing observations. Static reads cannot prove executable gates. Unmet necessary work stays open. No new work in the finish batch.",
			schema: ToolSchema{
				Type:                 "object",
				AdditionalProperties: rejectAdditionalProperties(),
				Properties: map[string]PropertyDef{
					"status": {
						Type: "string",
						// User-owned lifecycle states such as cancelled and archived
						// deliberately do not belong here. They are available only
						// through explicit gateway controls, never a model tool call.
						Description: "Task status. Cancellation and archival are user controls and are not valid finish_run outcomes.",
						Enum:        append([]string(nil), finishRunStatuses...),
					},
					"summary": {
						Type:        "string",
						Description: "Short user-facing summary. Optional when final_response is supplied; otherwise required for legacy summary-only outcomes.",
					},
					"final_response": {
						Type:        "string",
						Description: "Complete final reply, saved and shown without another generation after acceptance. Optional for legacy summary-only outcomes. Text alone proves neither completion nor authorization.",
					},
					"done": {
						Type:        "array",
						Description: "Completed items.",
						Items:       &PropertyDef{Type: "string"},
					},
					"next_steps": {
						Type:        "array",
						Description: "Remaining or suggested next steps.",
						Items:       &PropertyDef{Type: "string"},
					},
					"files": {
						Type:        "array",
						Description: "Files or artifacts touched.",
						Items:       &PropertyDef{Type: "string"},
					},
					"tests": {
						Type:        "array",
						Description: "Verification performed.",
						Items:       &PropertyDef{Type: "string"},
					},
					"risks": {
						Type:        "array",
						Description: "Known risks or blockers.",
						Items:       &PropertyDef{Type: "string"},
					},
					"need_approve": {
						Type:        "boolean",
						Description: "Whether user approval is needed.",
					},
					"assessments": conditionAssessmentsProperty(),
					"plan_patch":  finalPlanPatchProperty(),
				},
				Required: []string{"status"},
			},
			metadata: ToolMetadata{
				Category:  "task",
				RiskLevel: ToolRiskLow,
			},
		},
		store: store,
	}
	tool.legacySchema = tool.schema
	tool.schema = finishModelSchema(tool.schema)
	return tool
}

func (t *FinishRunTool) Execute(args map[string]interface{}) (string, error) {
	status := strings.TrimSpace(taskStringArg(args, "status"))
	if status == "" {
		return "", finishInputError(fmt.Errorf("status is required; choose one of %s", strings.Join(finishRunStatuses, ", ")),
			"tool_arguments_invalid", "Resend finish_run with an explicit status.")
	}
	if !validFinishRunStatus(status) {
		return "", finishInputError(fmt.Errorf("status must be one of %s", strings.Join(finishRunStatuses, ", ")),
			"tool_arguments_invalid", "Resend finish_run with one of the listed statuses.")
	}
	if status == "needs_approval" {
		status = "blocked"
		args["need_approve"] = true
	}
	summary := strings.TrimSpace(taskStringArg(args, "summary"))
	response := strings.TrimSpace(taskStringArg(args, "final_response"))
	if summary == "" {
		if response == "" {
			return "", finishInputError(fmt.Errorf("final_response or summary is required"),
				"tool_arguments_invalid", "Resend finish_run with final_response (or a summary).")
		}
		// This is a display index, never completion evidence or a replacement
		// for the full final response, whose paragraphs remain intact below.
		summary = truncateRunes(toSingleLine(response), 240)
	}
	out := finishOutcomeFields(args, status, summary, response)
	if handled, err := prepareAtomicFinish(args, out); handled || err != nil {
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(out)
		return string(data), err
	}
	key := planKey(args)
	projection := runPlanProjectionFromArgs(args)
	assessments, assessed, err := prepareOutcomeAssessments(args, status, projection)
	if err != nil {
		var inputError interface{ ConditionAssessmentInput() bool }
		if errors.As(err, &inputError) && inputError.ConditionAssessmentInput() {
			return "", finishInputError(err, "condition_assessment_invalid",
				"Correct the finding or its evidence references and send finish_run again. Observe again only if the existing evidence is stale or unavailable.")
		}
		var precondition interface{ CompletionPrecondition() bool }
		if errors.As(err, &precondition) && precondition.CompletionPrecondition() {
			return "", completionPreconditionFailure(err)
		}
		return "", err
	}
	if status == "done" && projection != nil && !assessed {
		if err := projection.ValidateCompletion(ContextFromArgs(args)); err != nil {
			var precondition interface{ CompletionPrecondition() bool }
			if errors.As(err, &precondition) && precondition.CompletionPrecondition() {
				return "", completionPreconditionFailure(err)
			}
			return "", err
		}
	}
	if status == "done" && projection == nil && t.store != nil {
		if plan, ok := t.store.Get(key); ok {
			var unresolved []string
			for _, step := range plan.Plan {
				if step.Status != "completed" && step.Status != "cancelled" {
					unresolved = append(unresolved, step.Step)
				}
			}
			if len(unresolved) > 0 {
				return "", completionPreconditionFailure(fmt.Errorf("successful run still has unresolved plan steps: %s; call update_plan with the complete plan snapshot before finish_run", strings.Join(unresolved, "; ")))
			}
		}
	}
	if len(assessments) > 0 {
		out["assessments"] = assessments
	}
	if reason := finishRunCompletionReason(status); reason != "" {
		out["completion_reason"] = reason
	}
	data, _ := json.Marshal(out)
	if t.store != nil {
		t.store.Purge(key)
	}
	return string(data), nil
}

func finishOutcomeFields(args map[string]interface{}, status, summary, response string) map[string]interface{} {
	out := map[string]interface{}{
		"status":       status,
		"summary":      summary,
		"done":         taskStringSliceArg(args, "done"),
		"next_steps":   taskStringSliceArg(args, "next_steps"),
		"files":        taskStringSliceArg(args, "files"),
		"tests":        taskStringSliceArg(args, "tests"),
		"risks":        taskStringSliceArg(args, "risks"),
		"need_approve": taskBoolArg(args, "need_approve"),
	}
	if response != "" {
		out["final_response"] = response
	}
	return out
}

// finishInputError reports a finish_run call refused before any effect because
// its own arguments are wrong. finish_run is capped at one call per Run, so the
// recovery contract (preparation, corrected_input, not_dispatched) is what lets
// the kernel return the uncommitted attempt; without it a correctable mistake
// would cost the Run its only way to close.
func finishInputError(cause error, code, hint string) error {
	return newStableToolRecoveryError(cause, code, "invalid_input", cause.Error(), hint,
		"preparation", "corrected_input", "not_dispatched", false)
}

func completionPreconditionFailure(err error) error {
	return newStableToolRecoveryError(err, "completion_precondition", "stale_precondition", err.Error(),
		"Resolve the outstanding plan or verification requirement. One corrected finish_run attempt is available after a changed plan or successful verification; unchanged retries are refused.",
		"preparation", "after_state_change", "not_dispatched", false)
}

func finishRunCompletionReason(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed":
		return "completed"
	case "waiting_external":
		return "waiting_external"
	case "waiting_user":
		return "waiting_user"
	case "blocked":
		return "blocked"
	case "failed":
		return "failed"
	default:
		return ""
	}
}

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
