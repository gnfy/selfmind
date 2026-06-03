package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIAdapterChatUsesNativeTools(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL

	resp, err := adapter.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read it"}},
		Tools: []ToolDefinition{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  map[string]interface{}{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools sent = %d, want 1", len(got.Tools))
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call-1" || resp.ToolCalls[0].Function != "read_file" {
		t.Fatalf("unexpected tool calls: %+v", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
}

func TestOpenAIAdapterStreamAccumulatesToolCalls(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"delta": map[string]interface{}{
						"tool_calls": []interface{}{
							map[string]interface{}{
								"index": 0,
								"id":    "call-1",
								"type":  "function",
								"function": map[string]interface{}{
									"name":      "read_file",
									"arguments": "{\"path\"",
								},
							},
						},
					},
				},
			},
		})
		writeSSE(t, w, map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"delta": map[string]interface{}{
						"tool_calls": []interface{}{
							map[string]interface{}{
								"index": 0,
								"function": map[string]interface{}{
									"arguments": ":\"README.md\"}",
								},
							},
						},
					},
				},
			},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL

	ch, err := adapter.StreamChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read it"}},
		Tools: []ToolDefinition{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  map[string]interface{}{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	var calls []ToolCall
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		calls = append(calls, event.ToolCalls...)
	}

	if !got.Stream {
		t.Fatalf("stream flag was not sent")
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools sent = %d, want 1", len(got.Tools))
	}
	if len(calls) != 1 || calls[0].ID != "call-1" || calls[0].Function != "read_file" || calls[0].Args != `{"path":"README.md"}` {
		t.Fatalf("unexpected streamed tool calls: %+v", calls)
	}
}

func TestAnthropicAdapterStreamChat(t *testing.T) {
	var got AnthropicRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"usage": map[string]interface{}{"input_tokens": 7},
			},
		})
		writeSSE(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": "hello "},
		})
		writeSSE(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": "world"},
		})
		writeSSE(t, w, map[string]interface{}{
			"type":  "message_delta",
			"usage": map[string]interface{}{"output_tokens": 3},
		})
		writeSSE(t, w, map[string]interface{}{"type": "message_stop"})
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.BaseURL = server.URL

	ch, err := adapter.StreamChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "say hello"}},
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	var content string
	var usage UsageStats
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		content += event.Content
		if event.Usage != nil {
			usage.InputTokens += event.Usage.InputTokens
			usage.OutputTokens += event.Usage.OutputTokens
		}
	}

	if !got.Stream {
		t.Fatal("stream flag was not sent")
	}
	if content != "hello world" {
		t.Fatalf("content = %q, want %q", content, "hello world")
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
}

func writeSSE(t *testing.T, w http.ResponseWriter, payload map[string]interface{}) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal SSE payload: %v", err)
	}
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
