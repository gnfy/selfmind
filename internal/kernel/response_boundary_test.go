package kernel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// Exercise the real loop with provider responses, including a separate final
// stream event: parseable arguments may arrive before the output-limit reason.
type boundaryProvider struct {
	responses     []llm.ChatResponse
	requests      []llm.ChatRequest
	nonStream     bool
	beforeRequest func(llm.ChatRequest)
	streamFault   func() (string, error)
}

func (p *boundaryProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "unused", nil
}

func (p *boundaryProvider) next(req llm.ChatRequest) llm.ChatResponse {
	if p.beforeRequest != nil {
		p.beforeRequest(req)
	}
	p.requests = append(p.requests, req)
	if len(p.requests) <= len(p.responses) {
		return p.responses[len(p.requests)-1]
	}
	return llm.ChatResponse{Content: "Finished.", FinishReason: "stop"}
}

func (p *boundaryProvider) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	resp := p.next(req)
	return &resp, nil
}

func (p *boundaryProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	if p.streamFault != nil {
		partial, err := p.streamFault()
		if err != nil {
			if partial == "" {
				return nil, err
			}
			ch := make(chan llm.StreamEvent, 2)
			ch <- llm.StreamEvent{Content: partial}
			ch <- llm.StreamEvent{Err: err}
			close(ch)
			return ch, nil
		}
	}
	if p.nonStream {
		return nil, io.ErrUnexpectedEOF
	}
	resp := p.next(req)
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Content: resp.Content, ToolCalls: resp.ToolCalls}
	ch <- llm.StreamEvent{FinishReason: resp.FinishReason}
	close(ch)
	return ch, nil
}

type boundaryBackend struct{ calls []string }

func (b *boundaryBackend) Dispatch(name string, _ map[string]interface{}) (string, error) {
	b.calls = append(b.calls, name)
	return "executed", nil
}
func (*boundaryBackend) GetToolDefinitions() []map[string]interface{} { return nil }

func TestOutputLimitedToolResponseDoesNotDispatch(t *testing.T) {
	for _, nonStream := range []bool{false, true} {
		name := "stream"
		if nonStream {
			name = "non_stream"
		}
		t.Run(name, func(t *testing.T) {
			provider := &boundaryProvider{nonStream: nonStream, responses: []llm.ChatResponse{{
				FinishReason: "length",
				ToolCalls:    []llm.ToolCall{{ID: "cut-off", Function: "write_file", Args: `{"path":"draft.txt","content":"partial"}`}},
			}}}
			backend := &boundaryBackend{}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 3, 1, nil)
			if _, _, err := agent.RunConversation(context.Background(), "user", "cli", "prepare the document"); err != nil {
				t.Fatal(err)
			}
			if len(backend.calls) != 0 {
				t.Fatalf("truncated response dispatched tools: %v", backend.calls)
			}
			if len(provider.requests) != 2 {
				t.Fatalf("requests = %d, want bounded correction", len(provider.requests))
			}
			found := false
			for _, msg := range provider.requests[1].Messages {
				if msg.Role == "tool" && msg.ToolCallID == "cut-off" && strings.Contains(msg.Content, "not executed") {
					found = true
				}
			}
			if !found {
				t.Fatal("next request has no paired not-executed result")
			}
		})
	}
}

func TestToolResponseCompletenessAcrossEncodings(t *testing.T) {
	for _, nonStream := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			for _, reason := range []string{"length", "max_tokens", "max_output_tokens", "output_limit", "stop", ""} {
				t.Run(fmt.Sprintf("fallback=%v/legacy=%v/%s", nonStream, legacy, reason), func(t *testing.T) {
					resp := llm.ChatResponse{FinishReason: reason}
					if legacy {
						resp.Content = `[TOOL:terminal:{"command":"echo example"}]`
					} else {
						resp.ToolCalls = []llm.ToolCall{
							{ID: "command", Function: "terminal", Args: `{"command":"echo example"}`},
							{ID: "edit", Function: "patch", Args: `{"patch":"example"}`},
						}
					}
					provider := &boundaryProvider{nonStream: nonStream, responses: []llm.ChatResponse{resp}}
					backend := &boundaryBackend{}
					agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 3, 1, nil)
					if _, _, err := agent.RunConversation(context.Background(), "user", "cli", "prepare the output"); err != nil {
						t.Fatal(err)
					}
					want := 2
					if legacy {
						want = 1
					}
					limited := reason != "stop" && reason != ""
					if limited {
						want = 0
					}
					if len(backend.calls) != want {
						t.Fatalf("dispatches = %v, want %d", backend.calls, want)
					}
					if len(provider.requests) != 2 {
						t.Fatalf("requests = %d, want 2", len(provider.requests))
					}
					if limited {
						paired := 0
						for _, msg := range provider.requests[1].Messages {
							if msg.Role == "tool" && strings.Contains(msg.Content, "effect_state: not_dispatched") {
								paired++
							}
						}
						wantPairs := 2
						if legacy {
							wantPairs = 1
						}
						if paired != wantPairs {
							t.Fatalf("paired refusals = %d, want %d", paired, wantPairs)
						}
					}
				})
			}
		}
	}
}

func TestTruncatedToolCallCanBeRegenerated(t *testing.T) {
	call := llm.ToolCall{ID: "first", Function: "write_file", Args: `{"path":"other.txt","content":"complete"}`}
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			provider := &boundaryProvider{responses: []llm.ChatResponse{{FinishReason: "length", ToolCalls: []llm.ToolCall{call}}}}
			second := call
			second.ID = "second"
			reason := "tool_calls"
			if persistent {
				reason = "length"
			}
			provider.responses = append(provider.responses, llm.ChatResponse{FinishReason: reason, ToolCalls: []llm.ToolCall{second}})
			backend := &boundaryBackend{}
			iterations := 3
			if persistent {
				iterations = 2
			}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", iterations, 1, nil)
			events := make(chan string, 128)
			answer, _, err := agent.RunConversation(WithEventChannel(context.Background(), events), "user", "cli", "prepare the document")
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if persistent {
				want = 0
			}
			if len(backend.calls) != want {
				t.Fatalf("dispatches = %v, want %d", backend.calls, want)
			}
			if len(provider.requests) != iterations {
				t.Fatalf("requests = %d, want %d", len(provider.requests), iterations)
			}
			if persistent {
				if !strings.Contains(answer, "No tools") {
					t.Fatalf("missing bounded failure: %q", answer)
				}
				incomplete := false
				for _, event := range drainAgentEvents(events) {
					if event.Type == "turn.completed" && event.Payload["completion_reason"] == "output_limit" {
						incomplete = true
					}
				}
				if !incomplete {
					t.Fatal("persistent truncation must remain incomplete")
				}
			}
		})
	}
}

type steeringSummarizer struct {
	fakeSummarizer
	onSummary func()
}

func (p *steeringSummarizer) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if p.onSummary != nil {
		p.onSummary()
	}
	return p.fakeSummarizer.Chat(ctx, req)
}

func TestSteeringReceivedDuringCompactionReachesNextRequest(t *testing.T) {
	steer := make(chan SteeringInput, 2)
	const correction = "Only inspect the remaining files; do not change them."
	sent, checked := false, false
	provider := &boundaryProvider{}
	for i := 0; i < 10; i++ {
		provider.responses = append(provider.responses, llm.ChatResponse{ToolCalls: []llm.ToolCall{{
			ID: strings.Repeat("x", i+1), Function: "read_file", Args: `{"path":"report.txt"}`,
		}}})
	}
	provider.beforeRequest = func(req llm.ChatRequest) {
		if !sent || checked {
			return
		}
		checked = true
		for _, id := range []string{"during-compaction", "same-text-new-id"} {
			found := false
			for _, msg := range req.Messages {
				if msg.Role == "user" && strings.Contains(msg.Content, correction) && strings.Contains(msg.Content, id) && strings.Contains(msg.Content, "attachment-ref") {
					found = true
				}
			}
			if !found {
				t.Errorf("first model request after compaction missed correction/attachment for %s", id)
			}
		}
	}
	summarizer := &steeringSummarizer{fakeSummarizer: fakeSummarizer{reply: "Continue inspecting the files."}}
	summarizer.onSummary = func() {
		if !sent {
			for _, id := range []string{"during-compaction", "same-text-new-id"} {
				steer <- SteeringInput{ID: id, Content: correction + "\nAttached file: attachment-ref", ContentHash: "correction"}
			}
			sent = true
		}
	}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &bigOutputBackend{}, provider, "helpful", 18, 1, nil)
	agent.SetContextWindow(6000)
	agent.SetSummaryProvider(summarizer)
	ctx := WithSteeringInputs(context.Background(), steer)
	ctx = WithWorkspaceContext(ctx, WorkspaceContext{ID: "ws", Root: t.TempDir()})
	if _, _, err := agent.RunConversation(ctx, "user", "cli", "inspect several files"); err != nil {
		t.Fatal(err)
	}
	if !sent || !checked {
		t.Fatal("test did not reach compaction followed by a model request")
	}
}

func TestSteeringDuringRecoveryRemainsInConversation(t *testing.T) {
	for _, partial := range []string{"", "Working on the answer. "} {
		t.Run(fmt.Sprintf("partial=%v", partial != ""), func(t *testing.T) {
			steer := make(chan SteeringInput, 1)
			provider := &boundaryProvider{responses: []llm.ChatResponse{{
				ToolCalls: []llm.ToolCall{{ID: "read", Function: "read_file", Args: `{"path":"report.txt"}`}},
			}}}
			sent := false
			provider.streamFault = func() (string, error) {
				if sent {
					return "", nil
				}
				sent = true
				steer <- SteeringInput{ID: "during-recovery", Content: "Only inspect the files."}
				return partial, io.ErrUnexpectedEOF
			}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &boundaryBackend{}, provider, "helpful", 3, 1, nil)
			if _, _, err := agent.RunConversation(WithSteeringInputs(context.Background(), steer), "user", "cli", "inspect the report"); err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(provider.requests))
			}
			for i, req := range provider.requests {
				found := false
				for _, msg := range req.Messages {
					if msg.Role == "user" && strings.Contains(msg.Content, "during-recovery") {
						found = true
					}
				}
				if !found {
					t.Errorf("request %d lost the correction accepted during recovery", i)
				}
			}
		})
	}
}

func TestBrokenLegacyStreamDoesNotDispatchItsCall(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprintf("complete=%v", complete), func(t *testing.T) {
			partial, continuation := `[TOOL:write_file:{"path":"draft.txt","content":"interrupted"}]`, "Finished."
			if !complete {
				partial, continuation = `[TOOL:write_file:{"path":"draft.txt","content":"`, `interrupted"}]`
			}
			provider := &boundaryProvider{responses: []llm.ChatResponse{{Content: continuation, FinishReason: "stop"}}}
			provider.streamFault = func() (string, error) { return partial, io.ErrUnexpectedEOF }
			backend := &boundaryBackend{}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 1, 1, nil)
			if _, _, err := agent.RunConversation(context.Background(), "user", "cli", "prepare the document"); err != nil {
				t.Fatal(err)
			}
			if len(backend.calls) != 0 {
				t.Fatalf("broken stream dispatched stale calls: %v", backend.calls)
			}
		})
	}
}

func TestOversizedLateSteeringIsNotAcknowledgedAsConsumed(t *testing.T) {
	provider := &boundaryProvider{}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &boundaryBackend{}, provider, "helpful", 2, 1, nil)
	agent.SetContextWindow(6000)
	steer := make(chan SteeringInput, 1)
	steer <- SteeringInput{ID: "oversized", Content: strings.Repeat("mandatory correction ", 12000)}
	events := make(chan string, 128)
	ctx := WithEventChannel(WithSteeringInputs(context.Background(), steer), events)
	_, _, err := agent.RunConversation(ctx, "user", "cli", "inspect the report")
	if err == nil || !strings.Contains(err.Error(), "input budget") {
		t.Fatalf("expected an actionable budget failure, got %v", err)
	}
	if len(provider.requests) != 0 {
		t.Fatal("oversized request reached the provider")
	}
	for _, event := range drainAgentEvents(events) {
		if event.Type == "agent.steering" {
			t.Fatal("unfitted input was acknowledged as consumed")
		}
	}
}
