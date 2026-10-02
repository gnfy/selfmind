package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func TestUnavailableToolCallReceivesPairedRefusal(t *testing.T) {
	for _, nonStream := range []bool{false, true} {
		provider := &boundaryProvider{nonStream: nonStream, responses: []llm.ChatResponse{
			{ToolCalls: []llm.ToolCall{{ID: "blocked", Function: "terminal", Args: `{}`}}},
			{Content: "The requested action remains unfinished."},
		}}
		backend := &boundaryBackend{}
		agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 3, 1, nil)
		events := make(chan string, 64)
		strategy := DefaultTaskStrategy()
		strategy.AllowedTools = map[string]bool{"verify": true, "update_plan": true, "finish_run": true}
		ctx := WithTaskStrategy(WithEventChannel(context.Background(), events), strategy)
		if _, _, err := agent.RunConversation(ctx, "test", "cli", "inspect the source"); err != nil {
			t.Fatal(err)
		}
		if len(backend.calls) != 0 {
			t.Fatalf("blocked tool was dispatched: %v", backend.calls)
		}
		if len(provider.requests) < 2 {
			t.Fatal("silent filtering ended the turn without feedback")
		}
		var call, result bool
		for _, msg := range provider.requests[1].Messages {
			for _, requested := range msg.ToolCalls {
				call = call || requested.ID == "blocked"
			}
			if msg.ToolCallID == "blocked" && msg.Role == "tool" {
				result = strings.Contains(msg.Content, "not_dispatched") && strings.Contains(msg.Content, "tool_not_available")
			}
		}
		if !call || !result {
			t.Fatalf("missing paired refusal (stream=%v): call=%v result=%v", !nonStream, call, result)
		}
		close(events)
		for raw := range events {
			if event, ok := DecodeAgentEvent(raw); ok && event.Type == "turn.completed" {
				if event.Payload["status"] != "incomplete" || event.Payload["resumable"] != true {
					t.Fatalf("refusal was presented as completion: %+v", event)
				}
			}
		}
	}
}

func TestAdmissionReservePreservesVerificationAndRefusalIdentity(t *testing.T) {
	strategy := DefaultTaskStrategy()
	strategy.MaxActionTools, strategy.ActionToolBudgetLimit, strategy.CompletionReserve = 8, 8, 2
	requested := []llm.ToolCall{{ID: "read", Function: "read_file"}, {ID: "check", Function: "verify"}, {ID: "close", Function: "finish_run"}}
	allowed, refused, dropped, _ := admitToolCalls(requested, strategy, 6, nil)
	if len(allowed) != 2 || allowed[0].ID != "check" || allowed[1].ID != "close" || len(refused) != 1 || refused[0].call.ID != "read" || dropped != 1 {
		t.Fatalf("reserve lost requested work or verification: allowed=%+v refused=%+v dropped=%d", allowed, refused, dropped)
	}
	if refused[0].err.ToolEffectState() != "not_dispatched" || refused[0].err.ToolStateChanged() {
		t.Fatal("refusal claimed an effect")
	}
	// More actual capacity changes admission; wording, paths, and model names do not.
	allowed, refused, dropped, _ = admitToolCalls(requested, strategy, 5, nil)
	if len(allowed) != 3 || len(refused) != 0 || dropped != 0 {
		t.Fatalf("available action capacity was ignored: %+v %+v %d", allowed, refused, dropped)
	}
}
