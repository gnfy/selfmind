package kernel

import (
	"context"
	"encoding/json"
	"strings"

	"selfmind/internal/kernel/llm"
)

// unresolvedPlanStepsFromResult reads the plan update_plan ACCEPTED. The
// durable projection keeps a requested but unassessed cancellation open, so the
// requested arguments overstate what is resolved: judging them let a Run finish
// done while its stored plan still had a pending step. A result without a
// readable plan is no evidence either way (ok=false): the caller keeps the last
// accepted list rather than failing open to the request.
func unresolvedPlanStepsFromResult(call llm.ToolCall, result toolExecutionResult) ([]string, bool) {
	return acceptRunPlanReceipt(nil, call, result)
}

// The same accepted receipt advances the completion view and execution cursor.
// A raw request, failed call or historical/transfer selection advances neither.
func acceptRunPlanReceipt(ctx context.Context, call llm.ToolCall, result toolExecutionResult) ([]string, bool) {
	if ctx != nil && !result.success {
		return nil, false
	}
	name := strings.TrimSpace(call.Function)
	if name != "update_plan" && name != "work_select" {
		return nil, false
	}
	var accepted struct {
		ExecutionRunID string `json:"execution_run_id"`
		Version        int    `json:"plan_version"`
		Status         string `json:"status"`
		CommitMode     string `json:"commit_mode"`
		Action         string `json:"action"`
		Plan           []struct {
			StepID string `json:"step_id"`
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(result.rawResult), &accepted); err != nil || accepted.Plan == nil {
		return nil, false
	}
	if name == "work_select" && (accepted.Status != "committed" || accepted.CommitMode != "direct" || accepted.Action != "resume") {
		return nil, false
	}
	if ctx != nil && name == "work_select" {
		if scope, ok := ToolInvocationScopeFromContext(ctx); ok && scope.RunID != "" && accepted.ExecutionRunID != "" && accepted.ExecutionRunID != scope.RunID {
			return nil, false
		}
	}
	if current, _ := CurrentRunExecutionPlan(ctx); accepted.Version > 0 && accepted.Version < current {
		return nil, false
	}
	currentStep := ""
	for _, step := range accepted.Plan {
		if step.Status == "in_progress" {
			currentStep = step.StepID
			break
		}
	}
	if accepted.Version > 0 {
		UpdateRunExecutionPlan(ctx, accepted.Version, currentStep)
	}
	unresolved := make([]string, 0, len(accepted.Plan))
	for _, step := range accepted.Plan {
		if status := strings.TrimSpace(step.Status); status == "completed" || status == "cancelled" {
			continue
		}
		name := strings.TrimSpace(step.Step)
		if name == "" {
			name = "unnamed step"
		}
		unresolved = append(unresolved, name)
	}
	return unresolved, true
}
