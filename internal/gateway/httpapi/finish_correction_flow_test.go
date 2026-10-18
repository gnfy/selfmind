package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
)

// runUnchecked sends the flow's request and returns the outcome even when the
// Run does not close.
func (d planFlowDaemon) runUnchecked() api.MessageResponse {
	response, _ := d.daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "plan-flow", WorkspaceID: d.ws.ID,
		Content: "Inspect input.txt and report the scope", ApprovalMode: "full-auto",
	})
	return response
}

// A finish whose evidence reference is unavailable is refused before dispatch;
// the one attempt finish_run has per Run is handed back once, the corrected call
// is accepted, and the Run closes with its answer. This is the RUQX-1272 chain
// (an unavailable reference, then finish_run hidden, then an interrupted Run) run
// through the real server, dispatcher, evidence resolver and kernel loop.
func TestFinishWithAnUnavailableEvidenceReferenceCanBeCorrectedOnceInARealRun(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, _ []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1:
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step": "Inspect sources", "status": "completed", "success_criteria": "Source was read"})}
		case 2:
			args, _ := json.Marshal(map[string]interface{}{
				"status": "done", "final_response": submittedFinalBody,
				"assessments": []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"toolcall-15-0"}, "reason": "The source was read in the previous Run."}},
			})
			return []llm.ToolCall{{ID: "finish-unavailable-reference", Function: "finish_run", Args: string(args)}}
		default:
			return []llm.ToolCall{finishCall()}
		}
	}
	d := newPlanFlowDaemon(t, provider)
	response := d.run(t)
	if got := provider.calls.Load(); got != 3 {
		t.Fatalf("model calls = %d, want plan, refused finish, corrected finish", got)
	}
	taskID := response.Task.ID
	if d.count(t, taskID, "tool.completed", "condition_assessment_invalid") != 1 {
		t.Fatal("the refused finish is not recorded with its stable code")
	}
	if d.count(t, taskID, "tool.completed", "tool_not_available") != 0 {
		t.Fatal("the corrected finish was hidden after the refusal")
	}
	if d.count(t, taskID, "run.outcome", `"status":"done"`) != 1 {
		t.Fatal("the corrected finish did not close the Run as done")
	}
}

// An identical invalid payload is not dispatched again. A changed, valid
// submission remains possible without manufacturing a new verification.
func TestRepeatedInvalidFinishDoesNotLockAChangedValidSubmission(t *testing.T) {
	provider := &receiptScriptedProvider{}
	bad := func(id string) llm.ToolCall {
		args, _ := json.Marshal(map[string]interface{}{
			"status": "done", "final_response": submittedFinalBody,
			"assessments": []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"toolcall-15-0"}, "reason": "Earlier."}},
		})
		return llm.ToolCall{ID: id, Function: "finish_run", Args: string(args)}
	}
	provider.script = func(n int32, _ []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1:
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step": "Inspect sources", "status": "completed", "success_criteria": "Source was read"})}
		case 2:
			return []llm.ToolCall{bad("finish-first-invalid")}
		case 3:
			return []llm.ToolCall{bad("finish-second-invalid")}
		default:
			return []llm.ToolCall{finishCall()}
		}
	}
	d := newPlanFlowDaemon(t, provider)
	response := d.runUnchecked()
	taskID := response.Task.ID
	if d.count(t, taskID, "tool.completed", "condition_assessment_invalid") != 1 || d.count(t, taskID, "tool.completed", "tool_arguments_repeated") != 1 {
		t.Fatal("identical input was dispatched again or lost its correction feedback")
	}
	if d.count(t, taskID, "run.outcome", `"status":"done"`) != 1 || provider.calls.Load() != 4 {
		t.Fatalf("changed valid submission could not finish: calls=%d done=%d response=%+v", provider.calls.Load(), d.count(t, taskID, "run.outcome", `"status":"done"`), response)
	}
}
