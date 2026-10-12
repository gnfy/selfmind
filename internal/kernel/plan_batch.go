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
}

func dispatchBoundaryFromPlan(raw string) (string, bool) {
	var receipt struct {
		Plan  []planDispatchStep `json:"plan"`
		Units []struct {
			ID, Goal string
			Status   string `json:"plan_status"`
		} `json:"work_units"`
	}
	if json.Unmarshal([]byte(raw), &receipt) != nil || receipt.Plan == nil {
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
	for _, step := range receipt.Plan {
		if step.Status != "in_progress" {
			continue
		}
		if step.ID == "" || step.UnitID != boundary.UnitID {
			return "", false
		}
		boundary.Steps = append(boundary.Steps, step)
	}
	if len(boundary.Steps) == 0 {
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
	if len(calls) < 2 || calls[0].Function != "update_plan" {
		results := a.executeToolCalls(ctx, tenantID, eventCh, calls)
		consume(calls, results)
		return results
	}
	before, known := latestDispatchBoundary(messages)
	results := make([]toolExecutionResult, 0, len(calls))
	stopped, unchanged := false, false
	for i, call := range calls {
		var result toolExecutionResult
		if stopped || i > 0 && !unchanged && call.Function != "finish_run" {
			result = a.toolDispatchRefused(eventCh, i, call, call.Function+"\x00"+call.Args,
				newPreparationPolicyError("plan_boundary_held", "policy_redirect", "after_state_change", "not_dispatched",
					"Plan boundary changed or could not be proven unchanged; this call was not executed. Re-issue only the actions needed under the accepted Plan", []string{"reassess_accepted_plan", "reissue_needed_calls"}))
			result.heldBeforeDispatch = true
		} else {
			result = a.executeToolCalls(ctx, tenantID, eventCh, calls[i:i+1])[0]
		}
		results = append(results, result)
		consume(calls[i:i+1], []toolExecutionResult{result})
		if call.Function == "update_plan" && !result.heldBeforeDispatch {
			after, readable := dispatchBoundaryFromPlan(result.rawResult)
			unchanged = known && readable && before == after
		}
		stopped = stopped || !result.success || result.pause != nil || call.Function == "finish_run"
	}
	return results
}
