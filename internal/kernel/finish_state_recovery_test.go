package kernel

import (
	"selfmind/internal/kernel/llm"
	"testing"
)

func TestFinishSecondGenuineCorrectionCanFinish(t *testing.T) {
	counts := map[string]int{"finish_run": 1}
	var correction finishCorrection
	failed := toolExecutionResult{toolName: "finish_run", errorCode: "completion_precondition", failurePhase: "preparation", retryability: "after_state_change", effectState: "not_dispatched"}
	correction.observe([]toolExecutionResult{failed, {toolName: "verify", success: true}}, counts)
	if counts["finish_run"] != 0 {
		t.Fatal("first genuine recovery remained capped")
	}
	counts["finish_run"]++
	correction.observe([]toolExecutionResult{failed, {toolName: "update_plan", success: true, rawResult: `{"changed":true}`}}, counts)
	accepted, refused, _, _ := admitToolCalls([]llm.ToolCall{{ID: "valid-finish", Function: "finish_run", Args: `{"status":"done","final_response":"Verified."}`}}, DefaultTaskStrategy(), 0, counts)
	if len(accepted) != 1 {
		t.Fatalf("second genuine correction cannot finish: counts=%v refused=%+v", counts, refused)
	}
}

func TestFinishStateRecoveryRemainsBounded(t *testing.T) {
	counts := map[string]int{}
	var correction finishCorrection
	for n := 0; n < 4; n++ {
		counts["finish_run"]++
		failed := toolExecutionResult{toolName: "finish_run", errorCode: "completion_precondition", failurePhase: "preparation", retryability: "after_state_change", effectState: "not_dispatched"}
		correction.observe([]toolExecutionResult{failed}, counts)
		correction.observe([]toolExecutionResult{{toolName: "update_plan", success: true, rawResult: `{"changed":false}`}}, counts)
		if counts["finish_run"] != 1 {
			t.Fatal("unchanged plan unlocked finish")
		}
		correction.observe([]toolExecutionResult{{toolName: "verify", success: false}}, counts)
		if counts["finish_run"] != 1 {
			t.Fatal("failed check unlocked finish")
		}
		correction.observe([]toolExecutionResult{{toolName: "verify", success: true}}, counts)
		want := 0
		if n == 3 {
			want = 1
		}
		if counts["finish_run"] != want {
			t.Fatalf("recovery %d got %v want %d", n+1, counts, want)
		}
	}
}

func TestFinishUnknownEffectsAndRejectedWorkNeverUnlockRecovery(t *testing.T) {
	for _, code := range []string{"completion_precondition", "condition_assessment_invalid", "approval_rejected"} {
		for _, effect := range []string{"", "unknown", "dispatched"} {
			counts := map[string]int{"finish_run": 1}
			var correction finishCorrection
			correction.observe([]toolExecutionResult{{toolName: "finish_run", errorCode: code, effectState: effect}, {toolName: "verify", success: true}}, counts)
			if counts["finish_run"] != 1 || correction.recoveries != 0 {
				t.Fatalf("%s/%s unlocked: %v", code, effect, counts)
			}
		}
	}
}
