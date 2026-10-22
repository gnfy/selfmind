package kernel

import (
	"context"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"testing"
)

type explicitFinishProvider struct {
	boundaryProvider
}

func (p *explicitFinishProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	if p.nonStream {
		return p.boundaryProvider.StreamChat(context.Background(), req)
	}
	resp := p.next(req)
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Content: resp.Content, Phase: resp.Phase, ToolCalls: resp.ToolCalls}
	ch <- llm.StreamEvent{FinishReason: resp.FinishReason}
	close(ch)
	return ch, nil
}

func TestExplicitFinalAnswerNeedsNoPostFinishRequest(t *testing.T) {
	for _, nonStream := range []bool{false, true} {
		for _, phase := range []llm.AssistantPhase{llm.AssistantPhaseFinalAnswer, llm.AssistantPhaseCommentary, llm.AssistantPhaseUnspecified} {
			t.Run(string(phase)+map[bool]string{false: "/stream", true: "/fallback"}[nonStream], func(t *testing.T) {
				provider := &explicitFinishProvider{boundaryProvider: boundaryProvider{nonStream: nonStream, responses: []llm.ChatResponse{{Content: "Checked the result.", Phase: phase, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}}}}}
				storage := &committedMessagesStorage{}
				agent := NewAgent(memory.NewMemoryManager(storage), &budgetClosureBackend{}, provider, "helpful", 4, 1, nil)
				events := make(chan string, 256)
				answer, _, err := agent.RunConversation(WithEventChannel(context.Background(), events), "person", "cli", "check and report")
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if phase == llm.AssistantPhaseFinalAnswer {
					want = 1
				}
				if len(provider.requests) != want {
					t.Fatalf("model requests=%d, want %d", len(provider.requests), want)
				}
				if phase == llm.AssistantPhaseFinalAnswer && answer != "Checked the result." {
					t.Fatalf("answer=%q", answer)
				}
				paired := false
				for _, message := range storage.messages {
					if message.Role == "tool" && message.ToolCallID == "finish" {
						paired = true
					}
				}
				if !paired {
					t.Fatal("fast finish lost the accepted tool receipt")
				}
				if payload := turnCompletedPayload(t, drainAgentEvents(events)); payload["status"] != "completed" {
					t.Fatalf("completion=%v", payload)
				}
			})
		}
	}
}

func TestExplicitAnswerCannotBypassRejectedFinish(t *testing.T) {
	provider := &explicitFinishProvider{boundaryProvider: boundaryProvider{responses: []llm.ChatResponse{{Content: "Claimed complete.", Phase: llm.AssistantPhaseFinalAnswer, ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}}}}}
	backend := &repairClosureBackend{}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 4, 1, nil)
	if _, _, err := agent.RunConversation(context.Background(), "person", "cli", "report"); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) < 2 || backend.attempts != 1 {
		t.Fatal("final prose bypassed the runtime completion guard")
	}
}

func TestFinalPhaseKeepsClosingRequestWhenResultIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name, prose, stop string
		calls             []llm.ToolCall
	}{
		{"missing-body", "", "tool_calls", []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}},
		{"action-batch", "Claimed complete.", "tool_calls", []llm.ToolCall{{ID: "read", Function: "read_file", Args: `{}`}, {ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}},
		{"refused-tail", "Claimed complete.", "tool_calls", []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}, {ID: "later", Function: "update_plan", Args: `{"plan":[]}`}}},
		{"cut-output", "Partial answer.", "length", []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &explicitFinishProvider{boundaryProvider: boundaryProvider{responses: []llm.ChatResponse{{Content: tc.prose, Phase: llm.AssistantPhaseFinalAnswer, FinishReason: tc.stop, ToolCalls: tc.calls}}}}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &budgetClosureBackend{}, provider, "helpful", 4, 1, nil)
			if _, _, err := agent.RunConversation(context.Background(), "person", "cli", "report"); err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) < 2 {
				t.Fatal("incomplete batch incorrectly skipped the closing request")
			}
		})
	}
}
