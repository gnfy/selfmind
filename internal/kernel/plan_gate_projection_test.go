package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// projectingPlanBackend answers update_plan the way the durable store does:
// with the accepted projection, which keeps an unassessed cancellation open.
type projectingPlanBackend struct {
	planningBackend
	keepOpen bool
}

func (b *projectingPlanBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name != "update_plan" {
		return "ok", nil
	}
	raw, _ := args["plan"].([]interface{})
	accepted := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		requested, _ := item.(map[string]interface{})
		step := map[string]interface{}{"step": requested["step"], "status": requested["status"]}
		if b.keepOpen && step["status"] == "cancelled" {
			step["status"], step["cancellation_disposition"] = "pending", "unfinished"
		}
		accepted = append(accepted, step)
	}
	data, err := json.Marshal(map[string]interface{}{"plan": accepted, "changed": true, "plan_version": 2})
	return string(data), err
}

// The completion gate judges the plan the store accepted, not the one the
// model asked for. A requested cancellation the store reopened is still open
// work, so the plan repair fires and the turn cannot complete.
func TestCompletionGateUsesTheAcceptedPlanProjection(t *testing.T) {
	for _, tc := range []struct {
		name, step, cancellation string
		keepOpen                 bool
		wantReason               string
	}{
		{"reopened unassessed cancellation", "verify in staging", `"status":"cancelled"`, true, "plan_unresolved"},
		{"reopened uncovered not_required", "publish the release notes", `"status":"cancelled","cancellation_disposition":"not_required","cancellation_reason":"not needed"`, true, "plan_unresolved"},
		// An accepted cancellation is resolved; the gate must not invent work.
		{"accepted cancellation", "verify in staging", `"status":"cancelled","cancellation_disposition":"not_required","cancellation_reason":"covered"`, false, "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &boundaryProvider{responses: []llm.ChatResponse{
				{ToolCalls: []llm.ToolCall{{ID: "plan", Function: "update_plan",
					Args: fmt.Sprintf(`{"plan":[{"step":"implement the change","status":"completed"},{"step":%q,%s}]}`, tc.step, tc.cancellation)}}},
				{Content: "All work is complete."},
				{Content: "All work is complete."},
				{Content: "All work is complete."},
			}}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &projectingPlanBackend{keepOpen: tc.keepOpen}, provider, "helpful", 8, 1, nil)
			events := make(chan string, 128)
			ctx := WithTaskStrategy(WithEventChannel(context.Background(), events), TaskStrategy{
				Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyRequired,
				MaxActionTools: 4, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 4, MaxBudgetExtensions: 1,
			})
			if _, _, err := agent.RunConversation(ctx, "user123", "cli", "implement the change and verify it in staging"); err != nil {
				t.Fatal(err)
			}
			close(events)
			repairNamedStep := false
			for _, request := range provider.requests {
				for _, msg := range request.Messages {
					if msg.Role == "user" && strings.Contains(msg.Content, "reconcile the visible plan") && strings.Contains(msg.Content, tc.step) {
						repairNamedStep = true
					}
				}
			}
			if repairNamedStep != (tc.wantReason == "plan_unresolved") {
				t.Fatalf("plan repair named the open step=%v, want %v", repairNamedStep, tc.wantReason == "plan_unresolved")
			}
			reason := ""
			for raw := range events {
				if event, ok := DecodeAgentEvent(raw); ok && event.Type == "turn.completed" {
					reason, _ = event.Payload["completion_reason"].(string)
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("completion reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// resultScriptBackend answers successive update_plan calls with scripted
// results, the way a replaced or wrapped update_plan might answer.
type resultScriptBackend struct {
	planningBackend
	results []string
	calls   int
}

func (b *resultScriptBackend) Dispatch(name string, _ map[string]interface{}) (string, error) {
	if name != "update_plan" {
		return "ok", nil
	}
	b.calls++
	if b.calls <= len(b.results) {
		return b.results[b.calls-1], nil
	}
	return "ok", nil
}

// With no accepted plan read yet, an unreadable result cannot leave the gate
// open. The requested snapshot is judged instead, and a requested
// cancellation counts as open because the runtime may keep it.
func TestCompletionGateFailsClosedBeforeAnyAcceptedPlanIsRead(t *testing.T) {
	for _, tc := range []struct {
		name, result, open, wantReason string
	}{
		{"not JSON", "ok", "pending", "plan_unresolved"},
		{"no plan in the result", `{"changed":true}`, "pending", "plan_unresolved"},
		{"invalid JSON", `{"plan":[`, "pending", "plan_unresolved"},
		{"requested cancellation", "ok", "cancelled", "plan_unresolved"},
		// Every requested step completed: nothing is left to hold open.
		{"requested completion", "ok", "completed", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &boundaryProvider{responses: []llm.ChatResponse{
				{ToolCalls: []llm.ToolCall{{ID: "plan-1", Function: "update_plan",
					Args: `{"plan":[{"step":"implement the change","status":"completed"},{"step":"verify in staging","status":"` + tc.open + `"}]}`}}},
				{Content: "All work is complete."},
				{Content: "All work is complete."},
				{Content: "All work is complete."},
			}}
			backend := &resultScriptBackend{results: []string{tc.result}}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 10, 1, nil)
			events := make(chan string, 128)
			ctx := WithTaskStrategy(WithEventChannel(context.Background(), events), TaskStrategy{
				Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyRequired,
				MaxActionTools: 4, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 4, MaxBudgetExtensions: 1,
			})
			if _, _, err := agent.RunConversation(ctx, "user123", "cli", "implement the change and verify it in staging"); err != nil {
				t.Fatal(err)
			}
			close(events)
			reason := ""
			for raw := range events {
				if event, ok := DecodeAgentEvent(raw); ok && event.Type == "turn.completed" {
					reason, _ = event.Payload["completion_reason"].(string)
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("completion reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// A plan result the gate cannot read is not evidence that the plan resolved.
// The last accepted projection stays authoritative; the requested arguments,
// which count every cancellation resolved, never replace it.
func TestCompletionGateKeepsTheLastAcceptedPlanWhenAResultIsUnreadable(t *testing.T) {
	accepted := `{"plan":[{"step":"implement the change","status":"completed"},{"step":"verify in staging","status":"pending"}],"changed":true}`
	for _, tc := range []struct {
		name, second, wantReason string
	}{
		{"not JSON", "ok", "plan_unresolved"},
		{"no plan in the result", `{"changed":true}`, "plan_unresolved"},
		// A readable projection that resolves the step does release the gate.
		{"readable resolution", `{"plan":[{"step":"implement the change","status":"completed"},{"step":"verify in staging","status":"completed"}],"changed":true}`, "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := `{"plan":[{"step":"implement the change","status":"completed"},{"step":"verify in staging","status":"cancelled"}]}`
			provider := &boundaryProvider{responses: []llm.ChatResponse{
				{ToolCalls: []llm.ToolCall{{ID: "plan-1", Function: "update_plan", Args: cancelled}}},
				{ToolCalls: []llm.ToolCall{{ID: "plan-2", Function: "update_plan", Args: cancelled}}},
				{Content: "All work is complete."},
				{Content: "All work is complete."},
				{Content: "All work is complete."},
			}}
			backend := &resultScriptBackend{results: []string{accepted, tc.second}}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 10, 1, nil)
			events := make(chan string, 128)
			ctx := WithTaskStrategy(WithEventChannel(context.Background(), events), TaskStrategy{
				Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyRequired,
				MaxActionTools: 4, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 4, MaxBudgetExtensions: 1,
			})
			if _, _, err := agent.RunConversation(ctx, "user123", "cli", "implement the change and verify it in staging"); err != nil {
				t.Fatal(err)
			}
			close(events)
			reason := ""
			for raw := range events {
				if event, ok := DecodeAgentEvent(raw); ok && event.Type == "turn.completed" {
					reason, _ = event.Payload["completion_reason"].(string)
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("completion reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
