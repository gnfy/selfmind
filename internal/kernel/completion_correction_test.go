package kernel

import (
	"selfmind/internal/kernel/llm"
	"testing"
)

func TestCompletionProtocolFinishPayloadCorrectionDoesNotRequireNewEvidence(t *testing.T) {
	counts := map[string]int{}
	var correction finishCorrection
	invalid := toolExecutionResult{toolName: "finish_run", errorCode: "condition_assessment_invalid", failurePhase: "preparation", retryability: "corrected_input", effectState: "not_dispatched"}
	for i := 0; i < 2; i++ {
		counts["finish_run"]++
		correction.observe([]toolExecutionResult{invalid}, counts)
	}
	accepted, refused, _, _ := admitToolCalls([]llm.ToolCall{{ID: "corrected-third", Function: "finish_run", Args: `{"status":"done","final_response":"Checked result."}`}}, DefaultTaskStrategy(), 0, counts)
	if len(accepted) != 1 {
		t.Fatalf("corrected not-dispatched payload cannot finish without unrelated work: counts=%v refused=%+v", counts, refused)
	}
}
func TestCompletionProtocolRealCompletionPreconditionStillNeedsNewState(t *testing.T) {
	counts := map[string]int{"finish_run": 1}
	var correction finishCorrection
	correction.observe([]toolExecutionResult{{toolName: "finish_run", errorCode: "completion_precondition", failurePhase: "preparation", retryability: "after_state_change", effectState: "not_dispatched"}}, counts)
	if counts["finish_run"] != 1 {
		t.Fatal("unmet completion was unlocked by formatting recovery")
	}
}
