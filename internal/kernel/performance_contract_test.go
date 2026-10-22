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

func TestStalePlanReminderRespectsTheAdvertisedOrdering(t *testing.T) {
	provider := &stalePlanProvider{commands: planStaleReminderThreshold + 1, openSteps: true}
	runStalePlanTurn(t, provider)
	for _, req := range provider.requests {
		for _, message := range req.Messages {
			if strings.Contains(message.Content, stalePlanWording) && strings.Contains(message.Content, "same response as your next action") {
				t.Fatal("runtime reminder contradicts the Plan tool's exclusive ordering contract")
			}
		}
	}
}

type declaredFinishBackend struct {
	budgetClosureBackend
	reject bool
}

func (b *declaredFinishBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name == "finish_run" {
		if b.reject {
			return "", fmt.Errorf("completion precondition rejected")
		}
		result, err := json.Marshal(map[string]interface{}{"status": "done", "summary": "checked", "final_response": args["final_response"]})
		return string(result), err
	}
	return b.budgetClosureBackend.Dispatch(name, args)
}

func TestDeclaredFinalResponseStillRequiresCompleteAcceptedBookkeeping(t *testing.T) {
	const body = "Submitted answer"
	finish := llm.ToolCall{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked","final_response":"Submitted answer"}`}
	read := llm.ToolCall{ID: "read", Function: "read_file", Args: `{"path":"source.txt"}`}
	for _, tc := range []struct {
		name   string
		calls  []llm.ToolCall
		reject bool
		stop   string
	}{
		{"rejected outcome", []llm.ToolCall{finish}, true, "tool_calls"},
		{"action mixed with outcome", []llm.ToolCall{read, finish}, false, "tool_calls"},
		{"call after outcome", []llm.ToolCall{finish, read}, false, "tool_calls"},
		{"cut output", []llm.ToolCall{finish}, false, "length"},
		{"missing explicit body", []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}, false, "tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &boundaryProvider{responses: []llm.ChatResponse{{FinishReason: tc.stop, ToolCalls: tc.calls}, {Content: "Generated closing reply", FinishReason: "stop"}}}
			agent := NewAgent(memory.NewMemoryManager(&committedMessagesStorage{}), &declaredFinishBackend{reject: tc.reject}, provider, "helpful", 6, 1, nil)
			answer, _, err := agent.RunConversation(context.Background(), "person", "cli", "check and report")
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) < 2 || answer == body {
				t.Fatalf("invalid shortcut: requests=%d answer=%q", len(provider.requests), answer)
			}
		})
	}
}

func TestDeclaredFinalResponseAvoidsASecondGenerationWithoutNativePhase(t *testing.T) {
	for _, nonStream := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "fallback"}[nonStream], func(t *testing.T) {
			const body = "The checks passed.\n\nThe requested change is ready."
			args, _ := json.Marshal(map[string]interface{}{"status": "done", "summary": "checked", "final_response": body})
			provider := &boundaryProvider{nonStream: nonStream, responses: []llm.ChatResponse{{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: string(args)}}}}}
			storage := &committedMessagesStorage{}
			agent := NewAgent(memory.NewMemoryManager(storage), &declaredFinishBackend{}, provider, "helpful", 4, 1, nil)
			answer, _, err := agent.RunConversation(context.Background(), "person", "cli", "check and report")
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != 1 || answer != body {
				t.Fatalf("requests=%d answer=%q; explicit accepted final response must not be regenerated", len(provider.requests), answer)
			}
		})
	}
}
