package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicAdapterParsesCacheUsageNonStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":900,"cache_creation_input_tokens":40}}`)
	}))
	defer server.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.BaseURL = server.URL

	resp, err := adapter.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	want := UsageStats{InputTokens: 12, OutputTokens: 3, CacheReadInputTokens: 900, CacheCreationInputTokens: 40}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestAnthropicAdapterParsesCacheUsageFromMessageStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeSSE(t, w, map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"usage": map[string]interface{}{
					"input_tokens":                7,
					"cache_read_input_tokens":     500,
					"cache_creation_input_tokens": 20,
				},
			},
		})
		writeSSE(t, w, map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": "hello"},
		})
		writeSSE(t, w, map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": "end_turn"},
			"usage": map[string]interface{}{"output_tokens": 2},
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
	var usage UsageStats
	for event := range ch {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		if event.Usage != nil {
			usage.InputTokens += event.Usage.InputTokens
			usage.OutputTokens += event.Usage.OutputTokens
			usage.CacheReadInputTokens += event.Usage.CacheReadInputTokens
			usage.CacheCreationInputTokens += event.Usage.CacheCreationInputTokens
		}
	}
	want := UsageStats{InputTokens: 7, OutputTokens: 2, CacheReadInputTokens: 500, CacheCreationInputTokens: 20}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

// TestAnthropicAdapterPromptCacheOffRequestUnchanged pins the default wire
// contract: without the PromptCache quirk the request bytes are byte-identical
// to the pre-quirk serialization (system stays a plain string / stays omitted,
// no cache_control anywhere).
func TestAnthropicAdapterPromptCacheOffRequestUnchanged(t *testing.T) {
	adapter := NewAnthropicAdapter("test-key")

	req := adapter.requestFromChat(ChatRequest{
		SystemPrompt: "sys",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "yo"},
			{Role: "user", Content: "again"},
		},
	}, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	want := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"again"}],"max_tokens":1024,"system":"sys"}`
	if string(body) != want {
		t.Fatalf("quirk-off request changed:\n got %s\nwant %s", body, want)
	}

	// No system prompt: the system field must stay omitted entirely.
	req = adapter.requestFromChat(ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, false)
	body, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(body), `"system"`) || strings.Contains(string(body), "cache_control") {
		t.Fatalf("quirk-off request leaked system/cache_control: %s", body)
	}
}

// TestAnthropicAdapterPromptCacheOnAttachesBreakpoints verifies the opt-in
// placement: one breakpoint on the last system block and one rolling
// breakpoint on the last content block of the most recent message before the
// final user message; never more than 4 total.
func TestAnthropicAdapterPromptCacheOnAttachesBreakpoints(t *testing.T) {
	adapter := NewAnthropicAdapter("test-key")
	adapter.Quirks = ProviderQuirks{PromptCache: true}

	req := adapter.requestFromChat(ChatRequest{
		SystemPrompt: "sys",
		Messages: []Message{
			{Role: "user", Content: "turn one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "turn two"},
		},
	}, false)

	systemBlocks, ok := req.SystemPrompt.([]interface{})
	if !ok || len(systemBlocks) != 1 {
		t.Fatalf("system = %#v, want one content block", req.SystemPrompt)
	}
	systemBlock := systemBlocks[0].(map[string]interface{})
	if systemBlock["text"] != "sys" || fmt.Sprint(systemBlock["cache_control"]) != "map[type:ephemeral]" {
		t.Fatalf("system block = %#v", systemBlock)
	}

	// Rolling breakpoint: the assistant message before the final user message.
	rolling, ok := req.Messages[1].Content.([]interface{})
	if !ok || len(rolling) != 1 {
		t.Fatalf("rolling message content = %#v, want one block", req.Messages[1].Content)
	}
	rollingBlock := rolling[0].(map[string]interface{})
	if rollingBlock["text"] != "answer one" || fmt.Sprint(rollingBlock["cache_control"]) != "map[type:ephemeral]" {
		t.Fatalf("rolling block = %#v", rollingBlock)
	}

	// The final user message stays a plain string (no breakpoint on it).
	if req.Messages[2].Content != "turn two" {
		t.Fatalf("final user content = %#v", req.Messages[2].Content)
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if got := strings.Count(string(body), `"cache_control"`); got != 2 || got > maxAnthropicCacheBreakpoints {
		t.Fatalf("cache_control breakpoints = %d, want 2 (max %d): %s", got, maxAnthropicCacheBreakpoints, body)
	}
}

// A single-user-message request has no prior turn to anchor a rolling
// breakpoint on; only the system block is marked.
func TestAnthropicAdapterPromptCacheFirstTurnOnlySystemBreakpoint(t *testing.T) {
	adapter := NewAnthropicAdapter("test-key")
	adapter.Quirks = ProviderQuirks{PromptCache: true}

	req := adapter.requestFromChat(ChatRequest{
		SystemPrompt: "sys",
		Messages:     []Message{{Role: "user", Content: "first"}},
	}, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if got := strings.Count(string(body), `"cache_control"`); got != 1 {
		t.Fatalf("cache_control breakpoints = %d, want 1: %s", got, body)
	}
	if req.Messages[0].Content != "first" {
		t.Fatalf("final user content = %#v", req.Messages[0].Content)
	}
}
