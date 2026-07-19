package kernel

import "testing"

// resolveTurnCompletion is the single source of truth for turn reporting. The
// precedence must match the pre-refactor branch order exactly, and a
// structured finish_run status must suppress the tool-budget-incomplete
// signal (the model declared done).
func TestResolveTurnCompletion(t *testing.T) {
	cases := []struct {
		name   string
		in     completionSignals
		status string
		reason string
		resume bool
	}{
		{"clean completion", completionSignals{}, "completed", "completed", false},
		{"output limit wins over all", completionSignals{OutputLimited: true, ToolBudgetExhausted: true, PlanUnresolved: true, IterationCapped: true}, "incomplete", "output_limit", true},
		{"budget exhausted no finish", completionSignals{ToolBudgetExhausted: true}, "incomplete", "tool_budget_exhausted", true},
		{"finish status suppresses budget signal", completionSignals{ToolBudgetExhausted: true, FinishStatus: "done"}, "completed", "completed", false},
		{"plan unresolved", completionSignals{PlanUnresolved: true}, "incomplete", "plan_unresolved", true},
		{"budget beats plan", completionSignals{ToolBudgetExhausted: true, PlanUnresolved: true}, "incomplete", "tool_budget_exhausted", true},
		{"iteration cap is lowest-precedence incomplete", completionSignals{IterationCapped: true}, "incomplete", "max_iterations", true},
		{"finish status alone completes", completionSignals{FinishStatus: "blocked", IterationCapped: true}, "incomplete", "max_iterations", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTurnCompletion(tc.in)
			if got.Status != tc.status || got.Reason != tc.reason || got.Resumable != tc.resume {
				t.Fatalf("resolveTurnCompletion(%+v) = %+v, want {%s %s %v}", tc.in, got, tc.status, tc.reason, tc.resume)
			}
		})
	}
}
