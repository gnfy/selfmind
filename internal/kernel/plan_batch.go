package kernel

import (
	"context"
	"encoding/json"
	"strings"

	"selfmind/internal/kernel/llm"
)

// planDispatchBoundary is read from accepted receipts, not requested ordinals.
// Changed identity, goal or acceptance bars require a fresh model decision.
type planDispatchBoundary struct {
	UnitID string
	Goal   string
	Steps  []planDispatchStep
}

type planDispatchStep struct {
	ID                   string `json:"step_id"`
	UnitID               string `json:"work_unit_id"`
	Step                 string `json:"step"`
	Criterion            string `json:"success_criteria"`
	VerificationRequired bool   `json:"verification_required"`
	Status               string `json:"status"`
	WorkUnit             bool   `json:"work_unit,omitempty"`
}

func dispatchBoundaryFromPlan(raw string) (string, bool) {
	var receipt struct {
		Plan             []planDispatchStep `json:"plan"`
		AcceptanceReview []string           `json:"acceptance_review"`
		Units            []struct {
			ID, Goal string
			Status   string `json:"plan_status"`
		} `json:"work_units"`
	}
	if json.Unmarshal([]byte(raw), &receipt) != nil || receipt.Plan == nil || len(receipt.AcceptanceReview) > 0 {
		return "", false
	}
	boundary := planDispatchBoundary{}
	for _, unit := range receipt.Units {
		if unit.Status == "in_progress" {
			if boundary.UnitID != "" || strings.TrimSpace(unit.ID) == "" || strings.TrimSpace(unit.Goal) == "" {
				return "", false
			}
			boundary.UnitID, boundary.Goal = unit.ID, unit.Goal
		}
	}
	if boundary.UnitID == "" {
		return "", false
	}
	unitID := ""
	active := 0
	for _, step := range receipt.Plan {
		// Accepted receipts name each contiguous Work Unit at its first step;
		// stored snapshots may repeat it. Canonicalize both representations.
		if step.WorkUnit && step.UnitID == "" {
			return "", false
		}
		if step.UnitID != "" {
			unitID = step.UnitID
		}
		if step.ID == "" || unitID == "" {
			return "", false
		}
		step.UnitID = unitID
		boundary.Steps = append(boundary.Steps, step)
		if step.Status == "in_progress" {
			if unitID != boundary.UnitID {
				return "", false
			}
			active++
		}
	}
	if active != 1 {
		return "", false
	}
	data, _ := json.Marshal(boundary)
	return string(data), true
}

func latestDispatchBoundary(messages []llm.Message) (string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "tool" && (messages[i].Name == "update_plan" || messages[i].Name == "work_select") {
			return dispatchBoundaryFromPlan(messages[i].Content)
		}
	}
	return "", false
}

// Ordinary read-only batches keep their parallel path. A leading Plan update
// is a sequential control transaction: account for it before rechecking each
// later tool's exposure and middleware. No sibling inherits dispatch authority.
func (a *Agent) executePlanAwareToolCalls(ctx context.Context, tenantID string, eventCh chan string, calls []llm.ToolCall, messages []llm.Message, consume func([]llm.ToolCall, []toolExecutionResult)) []toolExecutionResult {
	if ctx.Value(committedTurnReaderKey{}) != nil && !onlyBookkeepingCalls(calls) {
		for _, call := range calls {
			if call.Function != "finish_run" {
				continue
			}
			var proposal map[string]json.RawMessage
			if json.Unmarshal([]byte(call.Args), &proposal) != nil || proposal["plan_patch"] == nil && proposal["final_response"] == nil {
				continue
			}
			results := make([]toolExecutionResult, len(calls))
			for i, sibling := range calls {
				results[i] = a.toolDispatchRefused(eventCh, i, sibling, sibling.Function+"\x00"+sibling.Args,
					newPreparationPolicyError("completion_batch_held", "policy_redirect", "corrected_input", "not_dispatched", "Finish must assess observations already returned to Main. Submit work first, then one final result; no call in this mixed batch executed.", nil))
				results[i].heldBeforeDispatch = true
			}
			consume(calls, results)
			return results
		}
	}
	if len(calls) < 2 || calls[0].Function != "update_plan" {
		results := a.executeToolCalls(ctx, tenantID, eventCh, calls)
		consume(calls, results)
		return results
	}
	before, known := latestDispatchBoundary(messages)
	results := make([]toolExecutionResult, 0, len(calls))
	stopped, canContinue := false, false
	planUpdates := 0
	steering := steeringFromContext(ctx)
	for i, call := range calls {
		var result toolExecutionResult
		if stopped || len(steering.inputs) > 0 || len(steering.legacy) > 0 || i > 0 && !canContinue && call.Function != "finish_run" {
			result = a.toolDispatchRefused(eventCh, i, call, call.Function+"\x00"+call.Args,
				newPreparationPolicyError("plan_boundary_held", "policy_redirect", "after_state_change", "not_dispatched",
					"Plan scope changed, current input is pending, or safe progress could not be established; this call was not executed. Reassess the accepted Plan and current input before reissuing needed actions", []string{"reassess_accepted_plan", "reissue_needed_calls"}))
			result.heldBeforeDispatch = true
		} else {
			result = a.executeToolCalls(ctx, tenantID, eventCh, calls[i:i+1])[0]
		}
		results = append(results, result)
		consume(calls[i:i+1], []toolExecutionResult{result})
		if call.Function == "update_plan" && !result.heldBeforeDispatch {
			after, readable := dispatchBoundaryFromPlan(result.rawResult)
			planUpdates++
			canContinue = known && readable && planUpdates == 1 && allowsPlanProgress(before, after)
		}
		stopped = stopped || !result.success || result.pause != nil || call.Function == "finish_run"
	}
	return results
}
