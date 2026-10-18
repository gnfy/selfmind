package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
)

// Two distinct missing milestones are repaired through the real gateway and
// dispatcher. Earlier refusals must not hide the later valid atomic finish.
func TestFinishAfterTwoAcceptedStateCorrectionsCommitsOnce(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, receipt []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1:
			return []llm.ToolCall{planCall(n,
				map[string]interface{}{"step": "Describe scope", "status": "in_progress"},
				map[string]interface{}{"step": "Outline alternatives", "status": "pending"})}
		case 3:
			return []llm.ToolCall{patchFlowCall(n, true, []map[string]interface{}{
				{"step_id": receipt[0]["step_id"], "status": "completed"},
				{"step_id": receipt[1]["step_id"], "status": "in_progress"},
			})}
		case 5:
			return []llm.ToolCall{patchFlowCall(n, true, []map[string]interface{}{
				{"step_id": receipt[1]["step_id"], "status": "completed"},
			})}
		default:
			args, _ := json.Marshal(map[string]interface{}{"status": "done", "final_response": "Both milestones are complete."})
			return []llm.ToolCall{{ID: fmt.Sprintf("finish-%d", n), Function: "finish_run", Args: string(args)}}
		}
	}
	d := newPlanFlowDaemon(t, provider)
	response, code := d.daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "plan-flow", WorkspaceID: d.ws.ID,
		Content: "Describe the scope and outline alternatives, then report both milestones.", ApprovalMode: "full-auto",
	})
	if code != 200 || response.Run == nil || response.Run.Status != "done" || response.Content != "Both milestones are complete." {
		t.Fatalf("valid completion was lost after state corrections: %+v", response)
	}
	if provider.calls.Load() != 6 {
		t.Fatalf("unexpected extra recovery calls: %d", provider.calls.Load())
	}
	if d.count(t, response.Task.ID, "tool.completed", "completion_precondition") != 2 ||
		d.count(t, response.Task.ID, "tool.completed", "tool_attempt_limit") != 0 ||
		d.count(t, response.Task.ID, "run.outcome", `"status":"done"`) != 1 {
		t.Fatal("refusals, recovery or single result commit diverged")
	}
}
