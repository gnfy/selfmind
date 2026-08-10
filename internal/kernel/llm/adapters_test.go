package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicAdapterAcceptsKimiDirectStringContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"content":"maintenance ok","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.ProviderName = "kimi-coding"
	adapter.BaseURL = server.URL
	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "maintenance ok" || resp.FinishReason != "end_turn" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestAnthropicAdapterClassifiesHTTP200EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-request-id", "kimi-request-1")
		fmt.Fprint(w, `{"content":[],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":0}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.ProviderName = "kimi-coding"
	adapter.BaseURL = server.URL
	_, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err == nil {
		t.Fatal("empty HTTP 200 must be an explicit provider error")
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Class != ProviderErrorEmptyResponse || providerErr.RequestID != "kimi-request-1" {
		t.Fatalf("error = %#v", err)
	}
}

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

func TestOpenAIAdapterOmitsNilRequiredFromNativeToolSchema(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer server.Close()

	var required []string
	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL
	_, err := adapter.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "probe"}},
		Tools: []ToolDefinition{{
			Name: "delegate_task",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   required,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	function := got.Tools[0]["function"].(map[string]interface{})
	parameters := function["parameters"].(map[string]interface{})
	if required, exists := parameters["required"]; exists {
		t.Fatalf("required must be omitted, got %#v", required)
	}
}

func TestAnthropicAdapterOmitsNilRequiredFromNativeToolSchema(t *testing.T) {
	var required []string
	adapter := NewAnthropicAdapter("test-key")
	wire := adapter.requestFromChat(ChatRequest{
		Messages: []Message{{Role: "user", Content: "probe"}},
		Tools: []ToolDefinition{{
			Name: "delegate_task",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   required,
			},
		}},
	}, false)
	if len(wire.Tools) != 1 {
		t.Fatalf("tools = %d", len(wire.Tools))
	}
	if required, exists := wire.Tools[0].InputSchema["required"]; exists {
		t.Fatalf("required must be omitted, got %#v", required)
	}
}

func TestOpenAIAdapterRefreshesTokenAfterUnauthorized(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("content-type", "application/json")
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"token expired","code":"token_expired"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("old-token")
	adapter.BaseURL = server.URL
	adapter.TokenRefresher = func() string { return "fresh-token" }

	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if fmt.Sprint(seen) != "[Bearer old-token Bearer fresh-token]" {
		t.Fatalf("auth headers = %#v", seen)
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
			"delta": map[string]interface{}{"stop_reason": "max_tokens"},
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
	var finishReason string
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		content += event.Content
		if event.FinishReason != "" {
			finishReason = event.FinishReason
		}
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
	if finishReason != "max_tokens" {
		t.Fatalf("finishReason = %q, want max_tokens", finishReason)
	}
}

func TestAnthropicAdapterRefreshesTokenAfterUnauthorized(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, firstNonEmptyString(r.Header.Get("Authorization"), r.Header.Get("x-api-key")))
		w.Header().Set("content-type", "application/json")
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"token expired","code":"token_expired"}}`)
			return
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("old-token")
	adapter.BaseURL = server.URL
	adapter.TokenRefresher = func() string { return "fresh-token" }

	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if fmt.Sprint(seen) != "[old-token fresh-token]" {
		t.Fatalf("auth headers = %#v", seen)
	}
}

func TestAnthropicAdapterStreamRefreshesTokenAfterUnauthorized(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, firstNonEmptyString(r.Header.Get("Authorization"), r.Header.Get("x-api-key")))
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"token expired","code":"token_expired"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": "ok"},
		})
		writeSSE(t, w, map[string]interface{}{"type": "message_stop"})
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("old-token")
	adapter.BaseURL = server.URL
	adapter.TokenRefresher = func() string { return "fresh-token" }

	ch, err := adapter.StreamChat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}
	var content string
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		content += event.Content
	}
	if content != "ok" {
		t.Fatalf("content = %q", content)
	}
	if fmt.Sprint(seen) != "[old-token fresh-token]" {
		t.Fatalf("auth headers = %#v", seen)
	}
}

func TestAnthropicAdapterStreamChatAcceptsDataPrefixWithoutSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeSSENoSpace(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": "kimi "},
		})
		writeSSENoSpace(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": "ok"},
		})
		writeSSENoSpace(t, w, map[string]interface{}{"type": "message_stop"})
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
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		content += event.Content
	}

	if content != "kimi ok" {
		t.Fatalf("content = %q, want %q", content, "kimi ok")
	}
}

func TestAnthropicAdapterStreamToolUseStartsWithEmptyInput(t *testing.T) {
	var got AnthropicRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		writeSSENoSpace(t, w, map[string]interface{}{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    "toolu-1",
				"name":  "write_file",
				"input": map[string]interface{}{},
			},
		})
		writeSSENoSpace(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": "{\"path\"",
			},
		})
		writeSSENoSpace(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": ":\"main.go\"}",
			},
		})
		writeSSENoSpace(t, w, map[string]interface{}{"type": "message_stop"})
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.BaseURL = server.URL

	ch, err := adapter.StreamChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "write it"}},
		Tools: []ToolDefinition{{
			Name:        "write_file",
			Description: "Write a file",
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
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID != "toolu-1" || calls[0].Function != "write_file" || calls[0].Args != `{"path":"main.go"}` {
		t.Fatalf("unexpected streamed tool call: %+v", calls[0])
	}
}

func TestOpenAIAdapterStreamChatAcceptsDataPrefixWithoutSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, `data:{"choices":[{"delta":{"content":"open"}}]}`+"\n\n")
		fmt.Fprint(w, `data:{"choices":[{"delta":{"content":"ai"}}]}`+"\n\n")
		fmt.Fprint(w, "data:[DONE]\n\n")
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL

	ch, err := adapter.StreamChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "say hello"}},
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	var content string
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		content += event.Content
	}

	if content != "openai" {
		t.Fatalf("content = %q, want %q", content, "openai")
	}
}

func TestAnthropicAdapterUsesMiniMaxBearerAuthAndTools(t *testing.T) {
	var got AnthropicRequest
	var authHeader string
	var xAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		xAPIKey = r.Header.Get("x-api-key")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}],"usage":{"input_tokens":4,"output_tokens":5}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("mm-key")
	adapter.BaseURL = server.URL
	adapter.Headers = map[string]string{"User-Agent": "test"}
	adapter.Model = "MiniMax-M3"
	// Use a MiniMax-looking URL only for auth strategy; route the request to the test server.
	adapter.BaseURL = server.URL
	adapter.Headers["X-SelfMind-Provider-Base"] = "https://api.minimax.io/anthropic"
	adapter.BaseURL = "https://api.minimax.io/anthropic/v1/messages"
	adapter.BaseURL = server.URL
	// Directly assert the helper too, because httptest URLs do not look like MiniMax.
	req := httptest.NewRequest(http.MethodPost, "https://api.minimax.io/anthropic/v1/messages", nil)
	mini := NewAnthropicAdapter("mm-key")
	mini.BaseURL = "https://api.minimax.io/anthropic/v1/messages"
	mini.setHeaders(req, "mm-key")
	if req.Header.Get("Authorization") != "Bearer mm-key" || req.Header.Get("x-api-key") != "" {
		t.Fatalf("minimax auth headers authorization=%q x-api-key=%q", req.Header.Get("Authorization"), req.Header.Get("x-api-key"))
	}

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
	if authHeader == "" && xAPIKey == "" {
		t.Fatal("test server did not receive auth headers")
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function != "read_file" || resp.ToolCalls[0].Args != `{"path":"README.md"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}

func TestAnthropicAdapterKimiDefaultUserAgent(t *testing.T) {
	adapter := NewAnthropicAdapter("kimi-key")
	adapter.BaseURL = "https://api.kimi.com/coding/v1/messages"
	req := httptest.NewRequest(http.MethodPost, adapter.BaseURL, nil)
	adapter.setHeaders(req, "kimi-key")
	if got := req.Header.Get("User-Agent"); got != "claude-code/0.1.0" {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestAnthropicAdapterUsesExplicitProviderQuirks(t *testing.T) {
	var got AnthropicRequest
	var authHeader string
	var xAPIKey string
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		xAPIKey = r.Header.Get("x-api-key")
		userAgent = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.BaseURL = server.URL
	adapter.Quirks = ProviderQuirks{
		AuthHeader:   "bearer",
		UserAgent:    "selfmind-provider-test/1.0",
		ThinkingMode: "kimi",
	}

	if _, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if authHeader != "Bearer test-key" || xAPIKey != "" {
		t.Fatalf("auth headers authorization=%q x-api-key=%q", authHeader, xAPIKey)
	}
	if userAgent != "selfmind-provider-test/1.0" {
		t.Fatalf("User-Agent = %q", userAgent)
	}
	if got.Thinking != nil {
		t.Fatalf("thinking should be omitted for kimi mode: %#v", got.Thinking)
	}
}

func TestAnthropicAdapterDisablesHTTP2ForKimiQuirk(t *testing.T) {
	adapter := NewAnthropicAdapter("kimi-key")
	adapter.BaseURL = "https://api.kimi.com/coding/v1/messages"
	adapter.Quirks = ProviderQuirks{DisableHTTP2: true}

	client := adapter.httpClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if transport.TLSNextProto == nil {
		t.Fatal("TLSNextProto = nil, want empty map to disable HTTP/2")
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want explicit HTTP/1.1 ALPN")
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("NextProtos = %#v, want [http/1.1]", got)
	}
}

func TestAnthropicAdapterMovesSystemMessagesToTopLevel(t *testing.T) {
	adapter := NewAnthropicAdapter("test-key")
	req := adapter.requestFromChat(ChatRequest{
		SystemPrompt: "runtime system",
		Messages: []Message{
			{Role: "system", Content: "conversation system"},
			{Role: "user", Content: "hello"},
		},
	}, false)
	if req.SystemPrompt != "runtime system\n\nconversation system" {
		t.Fatalf("system = %q", req.SystemPrompt)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Fatalf("message role = %q, want user", req.Messages[0].Role)
	}
}

func TestAnthropicAdapterSanitizesKimiToolSchema(t *testing.T) {
	adapter := NewAnthropicAdapter("kimi-key")
	adapter.BaseURL = "https://api.kimi.com/coding/v1/messages"
	adapter.Model = "kimi-for-coding"

	req := adapter.requestFromChat(ChatRequest{
		Messages: []Message{{Role: "user", Content: "use a tool"}},
		Tools: []ToolDefinition{{
			Name: "complex_tool",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"missing_type": map[string]interface{}{"description": "needs inferred type"},
					"nullable": map[string]interface{}{
						"type": "string",
						"enum": []interface{}{nil, "", "ok"},
					},
					"maybe_string": map[string]interface{}{
						"type": "string",
						"anyOf": []interface{}{
							map[string]interface{}{"type": "string"},
							map[string]interface{}{"type": "null"},
						},
					},
					"ref_with_sibling": map[string]interface{}{
						"$ref":        "#/$defs/item",
						"description": "Moonshot rejects siblings",
					},
					"tuple_items": map[string]interface{}{
						"type": "array",
						"items": []interface{}{
							map[string]interface{}{"type": "string"},
							map[string]interface{}{"type": "integer"},
						},
					},
					"non_standard_nullable": map[string]interface{}{
						"nullable": true,
					},
				},
			},
		}},
	}, false)

	if len(req.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(req.Tools))
	}
	props := req.Tools[0].InputSchema["properties"].(map[string]interface{})
	if got := props["missing_type"].(map[string]interface{})["type"]; got != "string" {
		t.Fatalf("missing_type.type = %v", got)
	}
	if got := props["nullable"].(map[string]interface{})["enum"]; fmt.Sprint(got) != "[ok]" {
		t.Fatalf("nullable.enum = %v", got)
	}
	if _, ok := props["maybe_string"].(map[string]interface{})["anyOf"]; ok {
		t.Fatalf("maybe_string anyOf should be collapsed: %+v", props["maybe_string"])
	}
	if got := props["ref_with_sibling"].(map[string]interface{}); len(got) != 1 || got["$ref"] != "#/$defs/item" {
		t.Fatalf("ref_with_sibling = %+v", got)
	}
	items := props["tuple_items"].(map[string]interface{})["items"].(map[string]interface{})
	if got := items["type"]; got != "string" {
		t.Fatalf("tuple_items.items.type = %v", got)
	}
	if _, ok := props["non_standard_nullable"].(map[string]interface{})["nullable"]; ok {
		t.Fatalf("nullable keyword should be removed: %+v", props["non_standard_nullable"])
	}
}

func TestOpenAIAdapterSanitizesMoonshotToolSchemaFromQuirks(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("kimi-key")
	adapter.BaseURL = server.URL
	adapter.Model = "kimi-for-coding"
	adapter.Quirks = ProviderQuirks{ToolSchema: "moonshot"}

	_, err := adapter.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "use a tool"}},
		Tools: []ToolDefinition{{
			Name: "complex_tool",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"nullable": map[string]interface{}{
						"type": "string",
						"enum": []interface{}{nil, "ok"},
					},
					"missing_type": map[string]interface{}{"description": "infer type"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(got.Tools))
	}
	function, _ := got.Tools[0]["function"].(map[string]interface{})
	parameters, _ := function["parameters"].(map[string]interface{})
	props, _ := parameters["properties"].(map[string]interface{})
	if got := props["nullable"].(map[string]interface{})["enum"]; fmt.Sprint(got) != "[ok]" {
		t.Fatalf("nullable enum = %v", got)
	}
	if got := props["missing_type"].(map[string]interface{})["type"]; got != "string" {
		t.Fatalf("missing_type.type = %v", got)
	}
}

func TestOpenAIAdapterSendsKimiReasoningOptions(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("kimi-key")
	adapter.BaseURL = server.URL
	adapter.Model = "kimi-for-coding"
	adapter.MaxTokens = 32768
	adapter.ReasoningEffort = "medium"
	adapter.Thinking = map[string]interface{}{"type": "enabled"}

	if _, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if got.MaxTokens != 32768 || got.ReasoningEffort != "medium" {
		t.Fatalf("max/reasoning = %d/%q", got.MaxTokens, got.ReasoningEffort)
	}
	thinking, _ := got.Thinking.(map[string]interface{})
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", got.Thinking)
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

func writeSSENoSpace(t *testing.T, w http.ResponseWriter, payload map[string]interface{}) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal SSE payload: %v", err)
	}
	fmt.Fprintf(w, "data:%s\n\n", string(b))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
