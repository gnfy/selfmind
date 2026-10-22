package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

const progressPlanBefore = `{"plan":[{"step_id":"a","step":"Inspect","work_unit_id":"unit","status":"in_progress","success_criteria":"input is known"},{"step_id":"b","step":"Change","status":"pending","success_criteria":"requested change is present"}],"work_units":[{"id":"unit","goal":"Inspect and change","plan_status":"in_progress"}]}`
const progressPlanAfter = `{"plan":[{"step_id":"a","step":"Inspect","work_unit_id":"unit","status":"completed","success_criteria":"input is known"},{"step_id":"b","step":"Change","status":"in_progress","success_criteria":"requested change is present"}],"work_units":[{"id":"unit","goal":"Inspect and change","plan_status":"in_progress"}]}`

func TestPlanProgressBatchAllowsOnlyMonotonicSameScopeProgress(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		allow               bool
	}{
		{"forward", progressPlanBefore, progressPlanAfter, true},
		{"unresolved review", progressPlanBefore, strings.Replace(progressPlanAfter, `"plan":`, `"acceptance_review":["an obligation remains"],"plan":`, 1), false},
		{"missing group identity", progressPlanBefore, strings.Replace(progressPlanAfter, `"step_id":"b",`, `"step_id":"b","work_unit":true,`, 1), false},
		{"changed inactive criterion", progressPlanBefore, strings.Replace(progressPlanBefore, "requested change is present", "a different goal", 1), false},
		{"removed independent work", progressPlanBefore, batchPlanReceipt("unit", "Inspect and change", "Inspect", "input is known"), false},
		{"backward", progressPlanAfter, progressPlanBefore, false},
		{"cancelled", progressPlanBefore, strings.Replace(progressPlanAfter, `"status":"completed"`, `"status":"cancelled"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounted := false
			backend := &planBatchBackend{receipt: tc.after, accounted: &accounted}
			agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
			calls := []llm.ToolCall{{ID: "plan", Function: "update_plan", Args: `{}`}, {ID: "read", Function: "read_file", Args: `{"path":"source.txt"}`}}
			results := agent.executePlanAwareToolCalls(withToolActivationState(context.Background()), "person", nil, calls, []llm.Message{{Role: "tool", Name: "update_plan", Content: tc.before}}, func(c []llm.ToolCall, r []toolExecutionResult) {
				if c[0].Function == "update_plan" && r[0].success {
					accounted = true
				}
			})
			if results[1].success != tc.allow || (backend.actions == 1) != tc.allow {
				t.Fatalf("same-scope progress=%v actions=%d results=%+v", tc.allow, backend.actions, results)
			}
		})
	}
}

func TestPlanProgressBatchRetainsSteeringAndSecondUpdateBarriers(t *testing.T) {
	for _, kind := range []string{"steering", "second-plan"} {
		t.Run(kind, func(t *testing.T) {
			accounted := false
			backend := &twoPlansBackend{planBatchBackend: planBatchBackend{receipt: progressPlanAfter, accounted: &accounted}, receipts: []string{progressPlanAfter, progressPlanAfter}}
			agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
			input := make(chan SteeringInput, 1)
			ctx := withToolActivationState(WithSteeringInputs(context.Background(), input))
			calls := []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: `{}`}}
			if kind == "second-plan" {
				calls = append(calls, llm.ToolCall{ID: "p2", Function: "update_plan", Args: `{}`})
			}
			calls = append(calls, llm.ToolCall{ID: "read", Function: "read_file", Args: `{"path":"source.txt"}`})
			results := agent.executePlanAwareToolCalls(ctx, "person", nil, calls, []llm.Message{{Role: "tool", Name: "update_plan", Content: progressPlanBefore}}, func(c []llm.ToolCall, r []toolExecutionResult) {
				if c[0].Function == "update_plan" && r[0].success {
					accounted = true
					if kind == "steering" {
						input <- SteeringInput{ID: "correction", Content: "Use the revised scope"}
					}
				}
			})
			if backend.actions != 0 || !results[len(results)-1].heldBeforeDispatch {
				t.Fatalf("%s did not hold work: %+v", kind, results)
			}
			if kind == "steering" {
				pending := drainSteering(steeringFromContext(ctx))
				if len(pending) != 1 || pending[0].ID != "correction" {
					t.Fatal("dispatch consumed or lost the user's correction")
				}
			}
		})
	}
}
