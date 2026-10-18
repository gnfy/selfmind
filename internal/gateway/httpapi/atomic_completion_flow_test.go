package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
)

func TestFinalPlanPatchAndAnswerCloseInOneModelSubmission(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, plan []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1:
			return []llm.ToolCall{firstSnapshot(n)}
		case 2:
			return []llm.ToolCall{{ID: "read", Function: "read_file", Args: `{"path":"input.txt"}`}}
		default:
			patch := []map[string]interface{}{}
			for _, step := range plan {
				patch = append(patch, map[string]interface{}{"step_id": step["step_id"], "status": "completed"})
			}
			args, _ := json.Marshal(map[string]interface{}{"status": "done", "final_response": submittedFinalBody, "plan_patch": patch})
			return []llm.ToolCall{{ID: "close", Function: "finish_run", Args: string(args)}}
		}
	}
	flow := newPlanFlowDaemon(t, provider)
	response := flow.run(t)
	if provider.calls.Load() != 3 {
		t.Fatalf("needed %d model calls; want plan, actual read, one final submission", provider.calls.Load())
	}
	plan, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID)
	if err != nil || plan == nil || plan.Version != 2 {
		t.Fatalf("final Plan was not committed: %+v %v", plan, err)
	}
	for _, step := range plan.Steps {
		if step.Status != "completed" {
			t.Fatalf("final result disagrees with Plan: %+v", step)
		}
	}
	if flow.count(t, response.Task.ID, "run.outcome", `"final_response":`) != 1 {
		t.Fatal("atomic outcome lost its persisted final reply")
	}
	if flow.count(t, response.Task.ID, "run.outcome", `"status":"done"`) != 1 || flow.count(t, response.Task.ID, "run.finished", "") != 1 {
		t.Fatal("completion was omitted or duplicated")
	}
}

func TestReadOnlyAnswerCommitsWithoutInventingAPlan(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, _ []map[string]interface{}) []llm.ToolCall {
		if n == 1 {
			return []llm.ToolCall{{ID: "read", Function: "read_file", Args: `{"path":"input.txt"}`}}
		}
		args, _ := json.Marshal(map[string]interface{}{"status": "done", "final_response": submittedFinalBody})
		return []llm.ToolCall{{ID: "close", Function: "finish_run", Args: string(args)}}
	}
	flow := newPlanFlowDaemon(t, provider)
	response := flow.run(t)
	if provider.calls.Load() != 2 {
		t.Fatalf("read then reply required %d model calls", provider.calls.Load())
	}
	if plan, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID); err != nil || plan != nil {
		t.Fatalf("invented Plan: %+v %v", plan, err)
	}
	if flow.count(t, response.Task.ID, "run.outcome", `"final_response":`) != 1 || flow.count(t, response.Task.ID, "run.finished", "") != 1 {
		t.Fatal("body-only finalization omitted or duplicated its durable result")
	}
}

func TestBatchReadExposesTheExactObservationUsedByAtomicCompletion(t *testing.T) {
	provider := &receiptScriptedProvider{}
	var reference string
	provider.observe = func(request llm.ChatRequest) {
		for _, message := range request.Messages {
			if message.Name != "batch_read" {
				continue
			}
			var receipt struct {
				Items []struct {
					ToolCallID string `json:"tool_call_id"`
				} `json:"items"`
			}
			if err := json.Unmarshal([]byte(message.Content), &receipt); err != nil {
				t.Errorf("batch receipt: %v", err)
				return
			}
			if len(receipt.Items) == 1 {
				reference = receipt.Items[0].ToolCallID
			}
		}
	}
	provider.script = func(n int32, plan []map[string]interface{}) []llm.ToolCall {
		if n == 1 {
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step": "Inspect the requested source", "status": "in_progress", "success_criteria": "The source was inspected"})}
		}
		if n == 2 {
			return []llm.ToolCall{{ID: "batch-source", Function: "batch_read", Args: `{"operations":[{"tool":"read_file","path":"input.txt"}]}`}}
		}
		if reference != "batch-source:batch:1" {
			t.Errorf("model could not see the exact child observation: %q", reference)
		}
		args, _ := json.Marshal(map[string]interface{}{"status": "done", "final_response": submittedFinalBody, "plan_patch": []map[string]interface{}{{"step_id": plan[0]["step_id"], "status": "completed"}}, "assessments": []map[string]interface{}{{"step_id": plan[0]["step_id"], "result": "met", "evidence_refs": []string{reference}, "reason": "The successful file observation contains the requested source."}}})
		return []llm.ToolCall{{ID: "close", Function: "finish_run", Args: string(args)}}
	}
	flow := newPlanFlowDaemon(t, provider)
	response, code := flow.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "plan-flow", WorkspaceID: flow.ws.ID, Content: "Inspect input.txt and report the scope", ApprovalMode: "full-auto"})
	if code != 200 || !strings.HasPrefix(response.Content, submittedFinalBody) {
		t.Fatalf("batch final reply was lost: %d %+v", code, response)
	}
	if provider.calls.Load() != 3 || flow.count(t, response.Task.ID, "tool.started", `"tool":"read_file"`) != 1 || flow.count(t, response.Task.ID, "tool.started", `"tool":"verify"`) != 0 {
		t.Fatal("batch observation required duplicate reading or verification")
	}
	if response.Outcome == nil || response.Outcome.Status != "done" {
		t.Fatalf("observation was not accepted: %+v", response)
	}
}
