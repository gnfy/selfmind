package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func turnCompletedPayload(t *testing.T, events []AgentEvent) map[string]interface{} {
	t.Helper()
	for _, event := range events {
		if event.Type == "turn.completed" {
			return event.Payload
		}
	}
	t.Fatal("the turn reported no completion")
	return nil
}

func providerCallStopReasons(events []AgentEvent) []string {
	var reasons []string
	for _, event := range events {
		if event.Type == "provider.call.usage" {
			reasons = append(reasons, fmt.Sprintf("%v/%v", event.Payload["transport"], event.Payload["stop_reason"]))
		}
	}
	return reasons
}

// A reply the provider cut short is not a finished answer. Output limits and
// provider interruptions can be repaired by continuing; a filtered reply ends
// the turn unfinished, because continuing would only route around the filter.
func TestProviderStopReasonDecidesWhetherTheReplyFinished(t *testing.T) {
	for _, tc := range []struct {
		reason     string
		repeats    int
		iterations int
		requests   int
		status     string
		completion string
	}{
		{reason: "stop", iterations: 3, requests: 1, status: "completed", completion: "completed"},
		{reason: "", iterations: 3, requests: 1, status: "completed", completion: "completed"},
		{reason: "length", iterations: 3, requests: 2, status: "completed", completion: "completed"},
		{reason: "insufficient_system_resource", iterations: 3, requests: 2, status: "completed", completion: "completed"},
		{reason: "pause_turn", repeats: 2, iterations: 2, requests: 2, status: "incomplete", completion: "provider_interrupted"},
		{reason: "content_filter", iterations: 3, requests: 1, status: "incomplete", completion: "provider_filtered"},
		{reason: "SAFETY", iterations: 3, requests: 1, status: "incomplete", completion: "provider_filtered"},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.reason, tc.iterations), func(t *testing.T) {
			provider := &boundaryProvider{}
			for i := 0; i < max(tc.repeats, 1); i++ {
				provider.responses = append(provider.responses, llm.ChatResponse{Content: "The report begins", FinishReason: tc.reason})
			}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &boundaryBackend{}, provider, "helpful", tc.iterations, 1, nil)
			events := make(chan string, 256)
			answer, _, err := agent.RunConversation(WithEventChannel(context.Background(), events), "user", "cli", "write the report")
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != tc.requests {
				t.Fatalf("requests = %d, want %d", len(provider.requests), tc.requests)
			}
			if !strings.HasPrefix(answer, "The report begins") {
				t.Fatalf("the delivered part of the reply was lost: %q", answer)
			}
			if tc.requests > 1 && tc.status == "completed" && !strings.Contains(answer, "Finished.") {
				t.Fatalf("the continuation was not joined to the reply: %q", answer)
			}
			payload := turnCompletedPayload(t, drainAgentEvents(events))
			if payload["status"] != tc.status || payload["completion_reason"] != tc.completion {
				t.Fatalf("completion = %v/%v, want %s/%s", payload["status"], payload["completion_reason"], tc.status, tc.completion)
			}
			if tc.status == "incomplete" && payload["resumable"] != true {
				t.Fatal("an unfinished reply must be resumable")
			}
		})
	}
}

// Calls from a reply cut short never run, and the refusal names what cut it.
func TestCutToolRequestIsNotExecuted(t *testing.T) {
	for reason, cause := range map[string]string{"content_filter": "filtered", "pause_turn": "interrupted", "length": "output limit"} {
		t.Run(reason, func(t *testing.T) {
			provider := &boundaryProvider{responses: []llm.ChatResponse{{
				FinishReason: reason,
				ToolCalls:    []llm.ToolCall{{ID: "cut", Function: "write_file", Args: `{"path":"draft.txt","content":"partial"}`}},
			}}}
			backend := &boundaryBackend{}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 3, 1, nil)
			events := make(chan string, 256)
			answer, _, err := agent.RunConversation(WithEventChannel(context.Background(), events), "user", "cli", "prepare the document")
			if err != nil {
				t.Fatal(err)
			}
			if len(backend.calls) != 0 {
				t.Fatalf("a cut request ran: %v", backend.calls)
			}
			recorded := drainAgentEvents(events)
			refused := false
			for _, event := range recorded {
				if event.ToolCallID == "cut" && strings.Contains(event.Error, cause) {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("no refusal names %q", cause)
			}
			if reason != "content_filter" {
				return
			}
			// A filtered request is not retried: the turn ends unfinished and
			// the answer says why nothing ran.
			if len(provider.requests) != 1 || !strings.Contains(answer, "filtered") ||
				turnCompletedPayload(t, recorded)["completion_reason"] != "provider_filtered" {
				t.Fatalf("requests = %d answer = %q; want the turn to end unfinished", len(provider.requests), answer)
			}
		})
	}
}

// A stream that closes before its terminal event without a stop reason
// delivered a prefix. The loop continues from that prefix instead of
// presenting it as the answer, and the usage record names both calls.
func TestUnterminatedStreamContinuesFromItsPartialReply(t *testing.T) {
	for _, terminated := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminated=%v", terminated), func(t *testing.T) {
			var mu sync.Mutex
			var requests []llm.ChatRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Stream   bool          `json:"stream"`
					Messages []llm.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests = append(requests, llm.ChatRequest{Messages: body.Messages})
				mu.Unlock()
				if !body.Stream {
					w.Header().Set("content-type", "application/json")
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":" and ends here."},"finish_reason":"stop"}]}`)
					return
				}
				w.Header().Set("content-type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"The report begins\"}}]}\n\n")
				if terminated {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				}
			}))
			defer server.Close()
			adapter := llm.NewOpenAIAdapter("test-key")
			adapter.BaseURL = server.URL
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &boundaryBackend{}, adapter, "helpful", 3, 1, nil)
			events := make(chan string, 256)
			answer, _, err := agent.RunConversation(WithEventChannel(context.Background(), events), "user", "cli", "write the report")
			if err != nil {
				t.Fatal(err)
			}
			recorded := drainAgentEvents(events)
			reasons := strings.Join(providerCallStopReasons(recorded), ",")
			mu.Lock()
			defer mu.Unlock()
			if terminated {
				if answer != "The report begins" || len(requests) != 1 || reasons != "stream/complete" {
					t.Fatalf("a terminated reply was treated as damage: answer=%q requests=%d usage=%s", answer, len(requests), reasons)
				}
				return
			}
			if answer != "The report begins and ends here." || len(requests) != 2 {
				t.Fatalf("answer=%q requests=%d, want the prefix continued once", answer, len(requests))
			}
			if reasons != "stream/unterminated,non_stream/complete" {
				t.Fatalf("usage stop reasons = %s", reasons)
			}
			recovery := requests[1].Messages
			if len(recovery) < 2 || recovery[len(recovery)-2].Role != "assistant" || recovery[len(recovery)-2].Content != "The report begins" ||
				!strings.Contains(recovery[len(recovery)-1].Content, "Continue from the exact point") {
				t.Fatalf("the recovery request does not continue from the delivered prefix: %+v", recovery)
			}
			if payload := turnCompletedPayload(t, recorded); payload["status"] != "completed" {
				t.Fatalf("completion = %v", payload)
			}
		})
	}
}
