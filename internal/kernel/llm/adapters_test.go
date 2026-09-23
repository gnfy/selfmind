package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"},"extra_content":{"google":{"thought_signature":"signed-step"}}}]}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
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
	if got := string(resp.ToolCalls[0].ReplayMetadata); got != `{"google":{"thought_signature":"signed-step"}}` {
		t.Fatalf("replay metadata = %s", got)
	}
	if resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
}

func TestOpenAIAdapterReplaysOpaqueToolCallMetadata(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"done"}}]}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL
	_, err := adapter.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "inspect"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call-1", Function: "read_file", Args: `{}`,
				ReplayMetadata: json.RawMessage(`{"google":{"thought_signature":"signed-step"}}`),
			}}},
			{Role: "tool", ToolCallID: "call-1", Content: "ok"},
		},
		Tools: []ToolDefinition{{Name: "read_file", Parameters: map[string]interface{}{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) < 2 || len(got.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool call missing: %+v", got.Messages)
	}
	if metadata := string(got.Messages[1].ToolCalls[0].ExtraContent); metadata != `{"google":{"thought_signature":"signed-step"}}` {
		t.Fatalf("replayed metadata = %s", metadata)
	}
}

func TestFinalAnswerWithoutNewToolsKeepsPairedToolHistory(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "inspect"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Function: "read_file", Args: `{}`}}},
		{Role: "tool", ToolCallID: "call-1", Content: "verified"},
	}
	req := ChatRequest{Messages: messages}
	openai := openAIRequestFromChat("test-model", req, false)
	if len(openai.Tools) != 0 || len(openai.Messages) != 3 || len(openai.Messages[1].ToolCalls) != 1 ||
		openai.Messages[2].Role != "tool" || openai.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("OpenAI final-answer request broke the native pair: %+v", openai)
	}
	anthropic := (&AnthropicAdapter{}).requestFromChat(req, false)
	if len(anthropic.Tools) != 0 || len(anthropic.Messages) != 3 {
		t.Fatalf("Anthropic final-answer request lost history: %+v", anthropic)
	}
	assistant, _ := json.Marshal(anthropic.Messages[1].Content)
	result, _ := json.Marshal(anthropic.Messages[2].Content)
	if !strings.Contains(string(assistant), `"type":"tool_use"`) || !strings.Contains(string(result), `"type":"tool_result"`) {
		t.Fatalf("Anthropic final-answer request broke the native pair: assistant=%s result=%s", assistant, result)
	}
}

func TestDeepSeekUsageAndReasoningAreNormalized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"reasoning_content":"inspect first","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20,"completion_tokens_details":{"reasoning_tokens":12}}}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL
	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "inspect"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ReasoningContent != "inspect first" {
		t.Fatalf("reasoning_content = %q", resp.ReasoningContent)
	}
	want := UsageStats{
		InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 80,
		CacheMissInputTokens: 20, ReasoningOutputTokens: 12, CacheUsageReported: true,
	}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestDeepSeekThinkingToolLoopReplaysReasoningAndDerivesUserID(t *testing.T) {
	var got OpenAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"done"}}]}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL
	adapter.Model = "deepseek-v4-flash"
	adapter.ReasoningEffort = "xhigh"
	adapter.Quirks = ProviderQuirks{
		ThinkingMode: "deepseek", UserIdentityField: "user_id", SupportsTools: true,
	}
	ctx := WithModelContext(context.Background(), ModelContext{TenantID: "tenant-a", PersonID: "person-a"})
	var optionsProbe OpenAIRequest
	adapter.applyOptions(ctx, &optionsProbe, ChatRequest{})
	if optionsProbe.Thinking == nil {
		t.Fatal("DeepSeek defaults must enable thinking before serialization")
	}
	_, err := adapter.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "inspect"},
			{Role: "assistant", ReasoningContent: "need evidence", ToolCalls: []ToolCall{{ID: "call-1", Function: "read_file", Args: "{}"}}},
			{Role: "tool", ToolCallID: "call-1", Content: "evidence"},
		},
		Tools:   []ToolDefinition{{Name: "read_file", Parameters: map[string]interface{}{"type": "object"}}},
		Options: map[string]interface{}{"tool_choice": "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningEffort != "max" {
		t.Fatalf("reasoning_effort = %q, want max", got.ReasoningEffort)
	}
	thinking, _ := got.Thinking.(map[string]interface{})
	if thinking["type"] != "enabled" || got.ToolChoice != nil {
		t.Fatalf("thinking/tool_choice = %#v/%#v", got.Thinking, got.ToolChoice)
	}
	if got.UserID != StableProviderUserID(ctx) || got.UserID == "" || strings.Contains(got.UserID, "person-a") {
		t.Fatalf("derived user_id = %q", got.UserID)
	}
	if len(got.Messages) < 2 || got.Messages[1].ReasoningContent != "need evidence" {
		t.Fatalf("assistant reasoning was not replayed: %+v", got.Messages)
	}
}

func TestDeepSeekRequestCanDisableThinkingForMaintenance(t *testing.T) {
	adapter := NewOpenAIAdapter("test-key")
	adapter.ReasoningEffort = "high"
	adapter.Thinking = map[string]interface{}{"type": "enabled"}
	adapter.Quirks = ProviderQuirks{ThinkingMode: "deepseek"}
	var request OpenAIRequest
	adapter.applyOptions(context.Background(), &request, ChatRequest{
		Options: map[string]interface{}{"reasoning_effort": "none"},
	})
	if request.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want omitted", request.ReasoningEffort)
	}
	thinking, _ := request.Thinking.(map[string]interface{})
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want disabled", request.Thinking)
	}
}

func TestOpenAIExtraOptionsOverrideDerivedUserIDAndReachTransport(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("X-Vendor") != "selfmind-test" {
			t.Fatalf("extra header = %q", r.Header.Get("X-Vendor"))
		}
		if r.URL.Query().Get("api-version") != "2026-08-11" {
			t.Fatalf("extra query = %q", r.URL.RawQuery)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"done"}}]}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("test-key")
	adapter.BaseURL = server.URL
	adapter.Headers = map[string]string{"X-Vendor": "selfmind-test"}
	adapter.ExtraQuery = map[string]interface{}{"api-version": "2026-08-11"}
	adapter.ExtraBody = map[string]interface{}{
		"user_id":  "operator-selected-user",
		"metadata": map[string]interface{}{"source": "config"},
	}
	adapter.Quirks = ProviderQuirks{UserIdentityField: "user_id"}
	ctx := WithModelContext(context.Background(), ModelContext{TenantID: "tenant-a", PersonID: "person-a"})
	if _, err := adapter.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	if got["user_id"] != "operator-selected-user" {
		t.Fatalf("user_id = %#v", got["user_id"])
	}
	metadata, _ := got["metadata"].(map[string]interface{})
	if metadata["source"] != "config" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestAnthropicAdapterMapsReasoningAndOpaqueUserIdentity(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.BaseURL = server.URL
	adapter.Model = "claude-test"
	adapter.ReasoningEffort = "high"
	adapter.Quirks = ProviderQuirks{ThinkingMode: "anthropic", UserIdentityField: "auto"}
	ctx := WithModelContext(context.Background(), ModelContext{TenantID: "tenant-a", PersonID: "person-a"})
	if _, err := adapter.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "inspect"}}}); err != nil {
		t.Fatal(err)
	}
	metadata, _ := got["metadata"].(map[string]interface{})
	if userID, _ := metadata["user_id"].(string); !strings.HasPrefix(userID, "sm_") || strings.Contains(userID, "person-a") {
		t.Fatalf("metadata.user_id = %#v", metadata["user_id"])
	}
	thinking, _ := got["thinking"].(map[string]interface{})
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) {
		t.Fatalf("thinking = %#v", thinking)
	}
	if maxTokens, _ := got["max_tokens"].(float64); maxTokens <= 16384 {
		t.Fatalf("max_tokens = %.0f, must exceed thinking budget", maxTokens)
	}
}

func TestAnthropicExtraBodyOverridesAutomaticUserIdentity(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"usage":{}}`)
	}))
	defer server.Close()
	adapter := NewAnthropicAdapter("test-key")
	adapter.BaseURL = server.URL
	adapter.Quirks = ProviderQuirks{UserIdentityField: "auto"}
	adapter.ExtraBody = map[string]interface{}{"metadata": map[string]interface{}{"user_id": "operator-selected"}}
	ctx := WithModelContext(context.Background(), ModelContext{TenantID: "tenant-a", PersonID: "person-a"})
	if _, err := adapter.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	metadata := got["metadata"].(map[string]interface{})
	if metadata["user_id"] != "operator-selected" {
		t.Fatalf("metadata.user_id = %#v", metadata["user_id"])
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
								"extra_content": map[string]interface{}{
									"google": map[string]interface{}{"thought_signature": "stream-signed-step"},
								},
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
	if got := string(calls[0].ReplayMetadata); got != `{"google":{"thought_signature":"stream-signed-step"}}` {
		t.Fatalf("streamed replay metadata = %s", got)
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
	mini.Quirks = ProviderQuirks{AuthHeader: "bearer", ThinkingMode: "minimax"}
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
	adapter.Quirks = ProviderQuirks{UserAgent: "claude-code/0.1.0"}
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
	adapter.Quirks = ProviderQuirks{ToolSchema: "moonshot"}

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

// Gemini's OpenAI-compatible endpoint rejects unknown request fields, so an
// unconfigured thinking map must stay off the wire entirely rather than being
// boxed into the interface field as a typed nil and serialized as null.
func TestOpenAIAdapterOmitsUnconfiguredThinking(t *testing.T) {
	var raw map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer server.Close()

	adapter := NewOpenAIAdapter("google-key")
	adapter.BaseURL = server.URL
	adapter.Model = "gemini-3.7-flash"

	if _, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if _, ok := raw["thinking"]; ok {
		t.Fatalf("thinking present in payload: %#v", raw["thinking"])
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

// Attribution used to be set only by the OpenRouter adapter's own request
// builder, which the streaming path never reaches: StreamChat hands the call
// to a plain OpenAI adapter. Every streamed call therefore arrived
// unattributed. Configured headers still win over the defaults.
func TestOpenRouterStreamChatKeepsAttributionHeaders(t *testing.T) {
	for name, configured := range map[string]map[string]string{
		"defaults":      nil,
		"user override": {"x-title": "My Fork"},
	} {
		t.Run(name, func(t *testing.T) {
			var got http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("content-type", "text/event-stream")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()

			adapter := NewOpenRouterAdapter("test-key")
			adapter.BaseURL = server.URL
			adapter.Headers = configured
			ch, err := adapter.StreamChat(context.Background(), ChatRequest{
				Messages: []Message{{Role: "user", Content: "hello"}},
			})
			if err != nil {
				t.Fatalf("StreamChat failed: %v", err)
			}
			for range ch {
			}

			if referer := got.Get("HTTP-Referer"); referer != openRouterReferer {
				t.Fatalf("HTTP-Referer = %q, want %q", referer, openRouterReferer)
			}
			wantTitle := openRouterTitle
			if configured != nil {
				wantTitle = "My Fork"
			}
			if title := got.Get("X-Title"); title != wantTitle {
				t.Fatalf("X-Title = %q, want %q", title, wantTitle)
			}
		})
	}
}

// JSON mode is the Chat Completions standard for "answer with a JSON object";
// asking in the prompt is not one. The option is forwarded verbatim when set
// and absent from the wire otherwise, so prose callers see no change.
func TestOpenAIRequestForwardsResponseFormatOnlyWhenAsked(t *testing.T) {
	format := map[string]interface{}{"type": "json_object"}
	with := openAIRequestFromChat("m", ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Options:  map[string]interface{}{"response_format": format},
	}, false)
	got, _ := with.ResponseFormat.(map[string]interface{})
	if got["type"] != "json_object" {
		t.Fatalf("response_format = %#v, want json_object", with.ResponseFormat)
	}
	body, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"response_format":{"type":"json_object"}`) {
		t.Fatalf("wire body lacks response_format: %s", body)
	}

	without := openAIRequestFromChat("m", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, false)
	body, _ = json.Marshal(without)
	if without.ResponseFormat != nil || strings.Contains(string(body), "response_format") {
		t.Fatalf("response_format leaked into a request that did not ask for it: %s", body)
	}
}

// ChatRequest.SystemPrompt is part of the request contract. This adapter used
// to drop it, so every caller that set the field rather than a system-role
// message ran against OpenAI-compatible providers with no instructions:
// the maintenance analyzer produced 12-token replies for a week, and a small
// model asked (in the prompt it never received) for a JSON object answered
// with a YAML echo on every attempt.
func TestOpenAIRequestCarriesSystemPromptAsLeadingSystemMessage(t *testing.T) {
	got := openAIRequestFromChat("m", ChatRequest{
		SystemPrompt: "Return one JSON object only.",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	}, false)
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || contentString(got.Messages[0].Content) != "Return one JSON object only." {
		t.Fatalf("system prompt did not reach the wire: %+v", got.Messages)
	}
	if got.Messages[1].Role != "user" {
		t.Fatalf("user message displaced: %+v", got.Messages)
	}

	// The agent loop supplies its own system message; it must not get two.
	own := openAIRequestFromChat("m", ChatRequest{
		SystemPrompt: "duplicate",
		Messages:     []Message{{Role: "system", Content: "the real one"}, {Role: "user", Content: "hi"}},
	}, false)
	if len(own.Messages) != 2 || contentString(own.Messages[0].Content) != "the real one" {
		t.Fatalf("a caller-supplied system message was duplicated or replaced: %+v", own.Messages)
	}

	// No system prompt, no system message.
	none := openAIRequestFromChat("m", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, false)
	if len(none.Messages) != 1 || none.Messages[0].Role != "user" {
		t.Fatalf("an empty system prompt produced a message: %+v", none.Messages)
	}
}

// Disabling reasoning is encoded per provider. "Send nothing" only means "no
// reasoning" on a model that does not reason by default; one that does applies
// its own default and reasons anyway. This drives a real Chat through an HTTP
// server so the assertion is on the bytes that leave the process, after every
// later rewrite and omitempty marshalling.
func TestDisabledReasoningEncodingFollowsTheThinkingModeQuirk(t *testing.T) {
	// The level reaches the adapter either on the request or as the route's
	// configured default; "disabled" must be encoded the same way from both.
	wire := func(t *testing.T, fromRoute bool, mode, effort string) map[string]interface{} {
		t.Helper()
		var got map[string]interface{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			w.Header().Set("content-type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{}"}}]}`)
		}))
		defer server.Close()
		adapter := NewOpenAIAdapter("test-key")
		adapter.BaseURL = server.URL
		adapter.Quirks = ProviderQuirks{ThinkingMode: mode}
		req := ChatRequest{Messages: []Message{{Role: "user", Content: "verdict"}}}
		if fromRoute {
			adapter.ReasoningEffort = effort
		} else {
			req.Options = map[string]interface{}{"reasoning_effort": effort}
		}
		if _, err := adapter.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return got
	}

	for _, fromRoute := range []bool{false, true} {
		source := "request"
		if fromRoute {
			source = "route"
		}
		// The fix: a provider that declares effort_none receives the literal
		// value. Every spelling of "disabled" converges on it.
		for _, effort := range []string{"none", "off", "disabled", " NONE "} {
			body := wire(t, fromRoute, "effort_none", effort)
			if body["reasoning_effort"] != "none" {
				t.Fatalf("%s effort_none + %q: reasoning_effort = %#v, want \"none\"", source, effort, body["reasoning_effort"])
			}
			if _, present := body["thinking"]; present {
				t.Fatalf("%s effort_none + %q: no thinking object may be sent, got %#v", source, effort, body["thinking"])
			}
		}

		// The constraint that must not change: every other OpenAI-compatible
		// mode omits the parameter, because some endpoints reject a value they
		// do not list.
		for _, mode := range []string{"", "openai", "omit"} {
			body := wire(t, fromRoute, mode, "none")
			if _, present := body["reasoning_effort"]; present {
				t.Fatalf("%s mode %q must omit reasoning_effort when disabled, got %#v", source, mode, body["reasoning_effort"])
			}
			if _, present := body["thinking"]; present {
				t.Fatalf("%s mode %q must not invent a thinking object, got %#v", source, mode, body["thinking"])
			}
		}

		// DeepSeek keeps its own disabled encoding.
		body := wire(t, fromRoute, "deepseek", "none")
		if _, present := body["reasoning_effort"]; present {
			t.Fatalf("%s deepseek must omit reasoning_effort when disabled, got %#v", source, body["reasoning_effort"])
		}
		if thinking, _ := body["thinking"].(map[string]interface{}); thinking["type"] != "disabled" {
			t.Fatalf("%s deepseek must send thinking disabled, got %#v", source, body["thinking"])
		}

		// The quirk governs disabling only: any other level passes through
		// unchanged.
		for _, effort := range []string{"low", "high"} {
			if got := wire(t, fromRoute, "effort_none", effort)["reasoning_effort"]; got != effort {
				t.Fatalf("%s effort_none must pass %q through unchanged, got %#v", source, effort, got)
			}
		}
	}
}
