package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesAdapterUsesDynamicTokenGetter(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"output_text":"ok","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("old-token", server.URL, "gpt-test")
	adapter.KeyGetter = func() string { return "fresh-token" }
	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if auth != "Bearer fresh-token" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestResponsesAdapterSendsStoreFalseWhenConfigured(t *testing.T) {
	var body map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("decode body: %v\n%s", err, string(data))
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"output_text":"ok","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	store := false
	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	adapter.Store = &store
	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if got, ok := body["store"].(bool); !ok || got {
		t.Fatalf("store = %#v, want false", body["store"])
	}
}

func TestResponsesAdapterSendsReasoningEffortAndCustomHeaders(t *testing.T) {
	var body map[string]interface{}
	var accountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		accountID = r.Header.Get("chatgpt-account-id")
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"output_text":"ok","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	adapter.ReasoningEffort = "high"
	adapter.Headers = map[string]string{"chatgpt-account-id": "acc-123"}
	if _, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	reasoning, ok := body["reasoning"].(map[string]interface{})
	if !ok || reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v, want effort=high", body["reasoning"])
	}
	if accountID != "acc-123" {
		t.Fatalf("chatgpt-account-id header = %q, want acc-123", accountID)
	}
}

func TestResponsesAdapterChatUsesStreamWhenRequired(t *testing.T) {
	var body map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("decode body: %v\n%s", err, string(data))
		}
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "ok"})
		writeSSE(t, w, map[string]interface{}{"type": "response.completed"})
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	adapter.RequireStream = true
	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if got, ok := body["stream"].(bool); !ok || !got {
		t.Fatalf("stream = %#v, want true", body["stream"])
	}
}

func TestResponsesAdapterStreamsMessageFromOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type": "response.output_item.done",
			"item": map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": "hello from item"},
				},
			},
		})
		writeSSE(t, w, map[string]interface{}{"type": "response.completed"})
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
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
	if content != "hello from item" {
		t.Fatalf("content = %q", content)
	}
}

func TestResponsesAdapterStreamsMessageFromCompletedPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type": "response.completed",
			"response": map[string]interface{}{
				"status": "completed",
				"output": []interface{}{
					map[string]interface{}{
						"type": "message",
						"role": "assistant",
						"content": []interface{}{
							map[string]interface{}{"type": "output_text", "text": "hello from completed"},
						},
					},
				},
				"usage": map[string]interface{}{"input_tokens": 3, "output_tokens": 4},
			},
		})
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	adapter.RequireStream = true
	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "hello from completed" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestResponsesAdapterDoesNotDuplicateOutputTextAndMessageContent(t *testing.T) {
	payload := responsesResponse{OutputText: "same answer"}
	payload.Output = append(payload.Output, struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "output_text", Text: "same answer"}},
	})
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	resp := adapter.chatResponseFromResponses(payload)
	if resp.Content != "same answer" {
		t.Fatalf("content = %q", resp.Content)
	}
}

func TestResponsesAdapterEmitsToolActivityOnOutputItemAdded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type": "response.output_item.added",
			"item": map[string]interface{}{
				"type": "function_call",
				"name": "read_file",
			},
		})
		writeSSE(t, w, map[string]interface{}{
			"type": "response.output_item.done",
			"item": map[string]interface{}{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "read_file",
				"arguments": `{"path":"README.md"}`,
			},
		})
		writeSSE(t, w, map[string]interface{}{"type": "response.completed"})
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	ch, err := adapter.StreamChat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "read"}}})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}
	var sawActivity bool
	var calls []ToolCall
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		if event.EventType == "agent.thinking" && strings.Contains(event.Content, "read_file") {
			sawActivity = true
		}
		calls = append(calls, event.ToolCalls...)
	}
	if !sawActivity {
		t.Fatalf("tool activity event was not emitted")
	}
	if len(calls) != 1 || calls[0].Function != "read_file" {
		t.Fatalf("tool calls = %+v", calls)
	}
}

func TestResponsesAdapterNormalizesNilRequiredInToolSchema(t *testing.T) {
	var required []string
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	tools := adapter.responsesTools([]ToolDefinition{{
		Name:        "delegate_task",
		Description: "Delegate a task.",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
			"required":   required,
		},
	}})
	data, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	text := string(data)
	if strings.Contains(text, `"required":null`) {
		t.Fatalf("tool schema contains null required: %s", text)
	}
	if !strings.Contains(text, `"required":[]`) {
		t.Fatalf("tool schema should include empty required array: %s", text)
	}
	params, ok := tools[0]["parameters"].(map[string]interface{})
	if !ok {
		t.Fatalf("parameters = %T", tools[0]["parameters"])
	}
	if got, ok := params["required"].([]interface{}); !ok || len(got) != 0 {
		t.Fatalf("required = %#v, want empty array", params["required"])
	}
}

func TestResponsesAdapterSanitizesInvalidReplayToolNames(t *testing.T) {
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	wire := adapter.requestFromChat(ChatRequest{Messages: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       "tool_123",
			Function: "skill:repo.scan",
			Args:     `{"path":"."}`,
		}}},
	}}, true)

	if len(wire.Input) != 1 {
		t.Fatalf("input length = %d, want 1: %#v", len(wire.Input), wire.Input)
	}
	name := wire.Input[0].Name
	if name == "skill:repo.scan" {
		t.Fatalf("tool name was not sanitized: %q", name)
	}
	if !responsesToolNameValid(name) {
		t.Fatalf("tool name %q does not match Responses pattern", name)
	}
}

func TestResponsesAdapterReplaysAssistantToolCallsBeforeOutputs(t *testing.T) {
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	wire := adapter.requestFromChat(ChatRequest{Messages: []Message{
		{Role: "user", Content: "inspect"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       "tool_123",
			Function: "read_file",
			Args:     `{"path":"README.md"}`,
		}}},
		{Role: "tool", Name: "read_file", ToolCallID: "tool_123", Content: "contents"},
	}}, true)

	if len(wire.Input) != 3 {
		t.Fatalf("input length = %d, want 3: %#v", len(wire.Input), wire.Input)
	}
	call := wire.Input[1]
	if call.Type != "function_call" || call.CallID != "tool_123" || call.Name != "read_file" || call.Arguments != `{"path":"README.md"}` {
		t.Fatalf("function call item = %#v", call)
	}
	if call.ID != "fc_tool_123" {
		t.Fatalf("function call id = %q", call.ID)
	}
	if call.Status != "completed" {
		t.Fatalf("function call status = %q, want completed", call.Status)
	}
	output := wire.Input[2]
	if output.Type != "function_call_output" || output.CallID != "tool_123" || output.Output == nil || *output.Output != "contents" {
		t.Fatalf("function output item = %#v", output)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"type":"function_call"`) || !strings.Contains(text, `"type":"function_call_output"`) {
		t.Fatalf("request should contain call and output items: %s", text)
	}
	if strings.Index(text, `"type":"function_call"`) > strings.Index(text, `"type":"function_call_output"`) {
		t.Fatalf("function_call must be serialized before output: %s", text)
	}
}

func TestResponsesAdapterIncludesEmptyToolOutput(t *testing.T) {
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	wire := adapter.requestFromChat(ChatRequest{Messages: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       "tool_empty",
			Function: "update_plan",
			Args:     `{"plan":[]}`,
		}}},
		{Role: "tool", Name: "update_plan", ToolCallID: "tool_empty", Content: ""},
	}}, true)

	if len(wire.Input) != 2 {
		t.Fatalf("input length = %d, want 2: %#v", len(wire.Input), wire.Input)
	}
	output := wire.Input[1]
	if output.Type != "function_call_output" || output.CallID != "tool_empty" || output.Output == nil || *output.Output != "" {
		t.Fatalf("function output item = %#v", output)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if !strings.Contains(string(data), `"output":""`) {
		t.Fatalf("empty function output must be serialized: %s", string(data))
	}
}

func TestResponsesAdapterDropsOrphanToolOutput(t *testing.T) {
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	wire := adapter.requestFromChat(ChatRequest{Messages: []Message{
		{Role: "user", Content: "inspect"},
		{Role: "tool", Name: "read_file", ToolCallID: "tool_orphan", Content: "contents"},
	}}, true)

	if len(wire.Input) != 1 {
		t.Fatalf("input length = %d, want 1: %#v", len(wire.Input), wire.Input)
	}
	if wire.Input[0].Role != "user" || wire.Input[0].Content != "inspect" {
		t.Fatalf("first input = %#v", wire.Input[0])
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(data), "function_call_output") || strings.Contains(string(data), "tool_orphan") {
		t.Fatalf("orphan function output should not be serialized: %s", string(data))
	}
}

func TestResponsesAdapterConsumesEachToolCallOutputOnce(t *testing.T) {
	adapter := NewResponsesAdapter("token", "https://example.test", "gpt-test")
	wire := adapter.requestFromChat(ChatRequest{Messages: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       "tool_once",
			Function: "read_file",
			Args:     `{"path":"README.md"}`,
		}}},
		{Role: "tool", Name: "read_file", ToolCallID: "tool_once", Content: "first"},
		{Role: "tool", Name: "read_file", ToolCallID: "tool_once", Content: "duplicate"},
	}}, true)

	outputs := 0
	for _, item := range wire.Input {
		if item.Type == "function_call_output" {
			outputs++
			if item.Output == nil || *item.Output != "first" {
				t.Fatalf("output item = %#v, want first output only", item)
			}
		}
	}
	if outputs != 1 {
		t.Fatalf("function_call_output count = %d, want 1: %#v", outputs, wire.Input)
	}
}

func TestResponsesAdapterMapsSanitizedToolNamesBackFromStream(t *testing.T) {
	var requestBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(data, &requestBody); err != nil {
			t.Fatalf("decode body: %v\n%s", err, string(data))
		}
		tools, ok := requestBody["tools"].([]interface{})
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %#v", requestBody["tools"])
		}
		tool, ok := tools[0].(map[string]interface{})
		if !ok {
			t.Fatalf("tool = %#v", tools[0])
		}
		name, _ := tool["name"].(string)
		if name == "skill:repo.scan" || !responsesToolNameValid(name) {
			t.Fatalf("tool name = %q", name)
		}

		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type": "response.output_item.done",
			"item": map[string]interface{}{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      name,
				"arguments": `{"path":"."}`,
			},
		})
		writeSSE(t, w, map[string]interface{}{"type": "response.completed"})
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	ch, err := adapter.StreamChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "scan"}},
		Tools: []ToolDefinition{{
			Name:        "skill:repo.scan",
			Description: "Scan a repo.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
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
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function != "skill:repo.scan" {
		t.Fatalf("function = %q, want original name", calls[0].Function)
	}
}

func TestResponsesAdapterRefreshesTokenAfterExpired401(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("content-type", "application/json")
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"Provided authentication token is expired. Please try signing in again.","code":"token_expired"},"status":401}`)
			return
		}
		fmt.Fprint(w, `{"output_text":"ok after refresh","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	refreshes := 0
	adapter := NewResponsesAdapter("old-token", server.URL, "gpt-test")
	adapter.TokenRefresher = func() string {
		refreshes++
		return "fresh-token"
	}

	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "ok after refresh" {
		t.Fatalf("content = %q", resp.Content)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if fmt.Sprint(seen) != "[Bearer old-token Bearer fresh-token]" {
		t.Fatalf("auth headers = %#v", seen)
	}
}

func TestResponsesAdapterStreamRefreshesTokenAfterExpired401(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"Provided authentication token is expired. Please try signing in again.","code":"token_expired"},"status":401}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{"type": "response.output_text.delta", "delta": "ok"})
		writeSSE(t, w, map[string]interface{}{"type": "response.completed"})
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("old-token", server.URL, "gpt-test")
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
	if fmt.Sprint(seen) != "[Bearer old-token Bearer fresh-token]" {
		t.Fatalf("auth headers = %#v", seen)
	}
}

func TestResponsesAdapterTokenExpiredErrorIsActionable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Provided authentication token is expired. Please try signing in again.","code":"token_expired"},"status":401}`)
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("expired-token", server.URL, "gpt-test")
	_, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Chat succeeded, want token-expired error")
	}
	got := err.Error()
	if !strings.Contains(got, "Codex login expired") || !strings.Contains(got, "codex login") {
		t.Fatalf("error = %q", got)
	}
	if strings.Contains(got, `"error"`) || strings.Contains(got, `"token_expired"`) {
		t.Fatalf("error should not expose raw JSON: %q", got)
	}
}

func TestResponsesAdapterDetailErrorIsReadable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"detail":"Store must be set to false"}`)
	}))
	defer server.Close()

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	_, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	got := err.Error()
	if got != "responses API error 400: Store must be set to false" {
		t.Fatalf("error = %q", got)
	}
	if strings.Contains(got, `"detail"`) {
		t.Fatalf("error should not expose raw JSON: %q", got)
	}
}
