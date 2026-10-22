package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

type recoveryObservationTool struct {
	tools.Tool
	calls atomic.Int32
}

func (t *recoveryObservationTool) ExecuteResult(map[string]interface{}) (kernel.ToolDispatchResult, error) {
	t.calls.Add(1)
	code := 0
	invoked := true
	return kernel.ToolDispatchResult{Output: "synthetic remote observation: healthy", Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}}, nil
}

type observedRecovery struct {
	Version int `json:"accepted_plan_version"`
	Steps   []struct {
		StepID string `json:"step_id"`
		Status string `json:"status"`
	} `json:"accepted_steps"`
	Recovery []struct {
		StepID   string   `json:"step_id"`
		Action   string   `json:"action"`
		Replaces string   `json:"replaces"`
		CWD      string   `json:"cwd"`
		Refs     []string `json:"evidence_refs"`
	} `json:"recovery"`
}

// Synthetic provider and process receipts; real tools, evidence, gateway, rollback
// and finalizer. Main follows the actual refusal receipt instead of a prewired fix.
func TestFinishRecoveryReopensMultipleClosedCyclesWhileFinalStepStaysActive(t *testing.T) {
	provider := &receiptScriptedProvider{}
	observation := &recoveryObservationTool{Tool: tools.NewVerifyTool()}
	var recovery observedRecovery
	var ids []string
	var reportPath string
	var flow planFlowDaemon
	sawRefusal := false
	provider.observe = func(request llm.ChatRequest) {
		for _, m := range request.Messages {
			if m.Name != "finish_run" {
				continue
			}
			start := strings.Index(m.Content, `{"accepted_plan_version":`)
			if start < 0 {
				continue
			}
			if err := json.NewDecoder(strings.NewReader(m.Content[start:])).Decode(&recovery); err != nil {
				t.Errorf("receipt: %v", err)
				continue
			}
			sawRefusal = true
			if !strings.Contains(m.Content, "as pending") || !strings.Contains(m.Content, "mode=patch") {
				t.Errorf("model did not receive actionable reopening instructions: %s", m.Content)
			}
			if len(recovery.Recovery) != 2 {
				t.Errorf("two stale obligations missing from actual receipt: %s", m.Content)
			}
			if len(recovery.Steps) != 3 || recovery.Steps[2].Status != "in_progress" {
				t.Errorf("candidate progress escaped rollback: %+v", recovery)
			}
		}
	}
	call := func(id, name string, args interface{}) llm.ToolCall {
		raw, _ := json.Marshal(args)
		return llm.ToolCall{ID: id, Function: name, Args: string(raw)}
	}
	provider.script = func(n int32, plan []map[string]interface{}) []llm.ToolCall {
		if n == 1 {
			return []llm.ToolCall{planCall(n,
				map[string]interface{}{"step": "Observe remote A", "status": "in_progress", "success_criteria": "A healthy", "verification_required": true, "work_unit": true},
				map[string]interface{}{"step": "Observe remote B", "status": "pending", "success_criteria": "B healthy", "verification_required": true, "work_unit": true},
				map[string]interface{}{"step": "Record observations", "status": "pending", "work_unit": true})}
		}
		if len(plan) == 3 {
			ids = []string{plan[0]["step_id"].(string), plan[1]["step_id"].(string), plan[2]["step_id"].(string)}
		}
		switch n {
		case 2:
			return []llm.ToolCall{call("old-a", "verify", map[string]interface{}{"command": "printf healthy-A", "step_id": ids[0]}), call("old-b", "verify", map[string]interface{}{"command": "printf healthy-B", "step_id": ids[1]})}
		case 3:
			return []llm.ToolCall{call("close-observations", "update_plan", map[string]interface{}{"mode": "patch", "plan": []map[string]interface{}{{"step_id": ids[0], "status": "completed"}, {"step_id": ids[1], "status": "completed"}, {"step_id": ids[2], "status": "in_progress"}}})}
		case 4:
			return []llm.ToolCall{call("record", "write_file", map[string]interface{}{"path": reportPath, "content": "Observed both remote systems."})}
		case 6:
			if len(recovery.Recovery) != 2 {
				return []llm.ToolCall{call("no-recovery", "finish_run", map[string]interface{}{"status": "waiting_user", "final_response": "Recovery receipt missing"})}
			}
			var patch []map[string]interface{}
			for _, r := range recovery.Recovery {
				if r.Action != "reopen" {
					t.Errorf("wrong action: %+v", r)
				}
				patch = append(patch, map[string]interface{}{"step_id": r.StepID, "status": "pending"})
			}
			return []llm.ToolCall{call("recover-cycles", "update_plan", map[string]interface{}{"mode": "patch", "plan": patch})}
		case 7:
			var calls []llm.ToolCall
			for i, r := range recovery.Recovery {
				calls = append(calls, call(fmt.Sprintf("fresh-%d", i), "verify", map[string]interface{}{"command": "printf healthy-current", "step_id": r.StepID, "local_dependencies": []string{}, "replaces": r.Replaces, "cwd": r.CWD, "reason": "Observe the same unchanged condition using the current direct method"}))
			}
			return calls
		}
		refs := []string{"old-a", "old-b"}
		patch := []map[string]interface{}{{"step_id": ids[2], "status": "completed"}}
		if n >= 8 && len(recovery.Recovery) != 2 {
			return []llm.ToolCall{call("stop-test", "finish_run", map[string]interface{}{"status": "waiting_user", "final_response": "Recovery not actionable"})}
		}
		if n >= 8 {
			refs = []string{"fresh-0", "fresh-1"}
			for i := 0; i < 2; i++ {
				patch = append(patch, map[string]interface{}{"step_id": ids[i], "status": "completed"})
			}
		}
		var assessments []map[string]interface{}
		for i := 0; i < 2; i++ {
			step := ids[i]
			if n >= 8 {
				step = recovery.Recovery[i].StepID
			}
			assessments = append(assessments, map[string]interface{}{"step_id": step, "result": "met", "evidence_refs": []string{refs[i]}, "reason": "The direct remote observation confirmed health"})
		}
		return []llm.ToolCall{call(fmt.Sprintf("finish-%d", n), "finish_run", map[string]interface{}{"status": "done", "final_response": submittedFinalBody, "plan_patch": patch, "assessments": assessments})}
	}
	flow = newPlanFlowDaemon(t, provider, observation, tools.NewWriteFileTool())
	root, err := filepath.EvalSymlinks(flow.ws.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	reportPath = filepath.Join(root, "report.md")
	response, code := flow.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "plan-flow", WorkspaceID: flow.ws.ID, Content: "Observe two remote systems and record the findings", ApprovalMode: "full-auto"})
	if code != 200 || response.Outcome == nil || response.Outcome.Status != "done" || !strings.HasPrefix(response.Content, submittedFinalBody) {
		t.Fatalf("recovery did not complete: %d %+v", code, response)
	}
	if !sawRefusal || provider.calls.Load() != 8 || observation.calls.Load() != 4 {
		t.Fatalf("wrong recovery cost: receipt=%t main=%d observations=%d", sawRefusal, provider.calls.Load(), observation.calls.Load())
	}
	events, err := flow.store.ListTaskEvents(context.Background(), response.Task.ID, 300)
	if err != nil {
		t.Fatal(err)
	}
	classified := false
	for _, event := range events {
		if event.Type != "tool.completed" {
			continue
		}
		var receipt struct {
			ID    string `json:"tool_call_id"`
			Code  string `json:"error_code"`
			Retry string `json:"retryability"`
		}
		if json.Unmarshal(event.Payload, &receipt) == nil && receipt.ID == "finish-5" {
			classified = receipt.Code == "completion_precondition" && receipt.Retry == "after_state_change"
		}
	}
	if !classified {
		t.Fatal("actual message path still treated stale evidence as input correction")
	}

	if flow.count(t, response.Task.ID, "run.finished", "") != 1 || flow.count(t, response.Task.ID, "tool.started", `"tool":"write_file"`) != 1 {
		t.Fatal("recovery repeated effects or committed twice")
	}
}
