package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClassifyStopReasonAcrossProtocols(t *testing.T) {
	for want, raws := range map[StopReason][]string{
		StopComplete:    {"stop", "tool_calls", "end_turn", "tool_use", "completed", "STOP"},
		StopLength:      {"length", "max_tokens", "max_output_tokens", "MAX_TOKENS", "model_context_window_exceeded"},
		StopFiltered:    {"content_filter", "refusal", "SAFETY"},
		StopInterrupted: {"insufficient_system_resource", "aborted", "pause_turn", "incomplete", "error"},
		StopMissing:     {"", "  "},
		StopOther:       {"vendor_specific"},
	} {
		for _, raw := range raws {
			if got := ClassifyStopReason(raw); got != want {
				t.Errorf("ClassifyStopReason(%q) = %q, want %q", raw, got, want)
			}
		}
	}
	if !StopLength.Continuable() || !StopInterrupted.Continuable() || StopFiltered.Continuable() || StopMissing.Continuable() {
		t.Fatal("only replies cut by the output budget or halted by the provider can be continued")
	}
}

// Every protocol has a terminal event. A body that closes cleanly before it,
// without having reported a stop reason, delivered only a prefix. One that
// reported a stop reason is complete even when the terminal event is missing,
// and a terminated stream without a reason is not evidence of a cut.
func TestStreamsThatCloseBeforeTheirTerminalEventAreUnterminated(t *testing.T) {
	openAI := func(url string) (<-chan StreamEvent, error) {
		adapter := NewOpenAIAdapter("test-key")
		adapter.BaseURL = url
		return adapter.StreamChat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "report"}}})
	}
	responses := func(url string) (<-chan StreamEvent, error) {
		return NewResponsesAdapter("token", url, "gpt-test").StreamChat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "report"}}})
	}
	anthropic := func(url string) (<-chan StreamEvent, error) {
		adapter := NewAnthropicAdapter("test-key")
		adapter.BaseURL = url
		return adapter.StreamChat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "report"}}})
	}
	delta := func(text string) map[string]interface{} {
		return map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": text}}}}
	}
	finish := func(reason string) map[string]interface{} {
		return map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{}, "finish_reason": reason}}}
	}
	anthropicText := func(text string) map[string]interface{} {
		return map[string]interface{}{"type": "content_block_delta", "delta": map[string]interface{}{"type": "text_delta", "text": text}}
	}
	for _, tc := range []struct {
		name       string
		stream     func(string) (<-chan StreamEvent, error)
		serve      func(*testing.T, http.ResponseWriter)
		cut        bool
		wantFinish string
		wantCalls  int
	}{
		{name: "openai closes mid-reply", stream: openAI, cut: true, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, delta("The report begins"))
		}},
		{name: "openai reports stop without DONE", stream: openAI, wantFinish: "stop", serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, delta("The whole report."))
			writeSSE(t, w, finish("stop"))
		}},
		{name: "openai announces tool calls without DONE", stream: openAI, wantFinish: "tool_calls", wantCalls: 1, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{"index": 0, "id": "call-1", "type": "function",
					"function": map[string]interface{}{"name": "read_file", "arguments": `{"path":"a.txt"}`}}}}}}})
			writeSSE(t, w, finish("tool_calls"))
		}},
		{name: "openai DONE without a trailing newline", stream: openAI, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, delta("The whole report."))
			fmt.Fprint(w, "data: [DONE]")
		}},
		{name: "responses closes mid-reply", stream: responses, cut: true, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "The report begins"})
		}},
		{name: "responses reports an incomplete response", stream: responses, wantFinish: "max_output_tokens", serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "The report begins"})
			writeSSE(t, w, map[string]interface{}{"type": "response.incomplete", "response": map[string]interface{}{
				"status": "incomplete", "incomplete_details": map[string]interface{}{"reason": "max_output_tokens"}}})
		}},
		{name: "responses completes", stream: responses, wantFinish: "completed", serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "The whole report."})
			writeSSE(t, w, map[string]interface{}{"type": "response.completed", "response": map[string]interface{}{"status": "completed"}})
		}},
		{name: "responses DONE without a terminal response event", stream: responses, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "The whole report."})
			fmt.Fprint(w, "data: [DONE]\n\n")
		}},
		{name: "responses terminal event without a trailing newline", stream: responses, wantFinish: "completed", serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "The whole report."})
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`)
		}},
		{name: "anthropic message_stop without a trailing newline", stream: anthropic, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, anthropicText("The whole report."))
			fmt.Fprint(w, `data: {"type":"message_stop"}`)
		}},
		{name: "anthropic message_stop without a stop reason", stream: anthropic, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, anthropicText("The whole report."))
			writeSSE(t, w, map[string]interface{}{"type": "message_stop"})
		}},
		{name: "anthropic closes mid-reply", stream: anthropic, cut: true, serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, anthropicText("The report begins"))
		}},
		{name: "anthropic reports a stop reason without message_stop", stream: anthropic, wantFinish: "end_turn", serve: func(t *testing.T, w http.ResponseWriter) {
			writeSSE(t, w, anthropicText("The whole report."))
			writeSSE(t, w, map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": "end_turn"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				tc.serve(t, w)
			}))
			defer server.Close()
			ch, err := tc.stream(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			var content, finishReason string
			var calls int
			var streamErr error
			for event := range ch {
				content += event.Content
				calls += len(event.ToolCalls)
				if event.FinishReason != "" {
					finishReason = event.FinishReason
				}
				if event.Err != nil {
					streamErr = event.Err
				}
			}
			if tc.cut {
				if !IsStreamUnterminated(streamErr) || !IsRetryableError(streamErr) {
					t.Fatalf("a reply cut before the terminal event must be retryable transport damage: %v", streamErr)
				}
				if content != "The report begins" {
					t.Fatalf("the delivered prefix was lost: %q", content)
				}
				return
			}
			if streamErr != nil {
				t.Fatalf("a terminated reply was reported as damage: %v", streamErr)
			}
			if finishReason != tc.wantFinish || calls != tc.wantCalls {
				t.Fatalf("finish=%q calls=%d, want finish=%q calls=%d", finishReason, calls, tc.wantFinish, tc.wantCalls)
			}
		})
	}
}
