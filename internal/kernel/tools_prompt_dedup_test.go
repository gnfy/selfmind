package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// nativeToolsProvider is a stub provider that reports native tool support.
type nativeToolsProvider struct{}

func (p *nativeToolsProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "", nil
}
func (p *nativeToolsProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{}, nil
}
func (p *nativeToolsProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}
func (p *nativeToolsProvider) SupportsNativeTools() bool { return true }

// textOnlyProvider does NOT implement the capability probe (no
// SupportsNativeTools method) — the safe default is the full text catalog.
type textOnlyProvider struct{}

func (p *textOnlyProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "", nil
}
func (p *textOnlyProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{}, nil
}
func (p *textOnlyProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// promptToolBackend registers one tool with a parameter schema so the test can
// assert whether the schema text leaks into the system prompt.
type promptToolBackend struct{}

func (promptToolBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	return "", nil
}

func (promptToolBackend) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{{
		"name":        "read_file",
		"description": "Read a file from the workspace.",
		"parameters": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "PARAM-SCHEMA-MARKER path to read"},
			},
		},
	}}
}

// TestToolPromptDedupForNativeProviders pins P1-1: a provider that carries
// ChatRequest.Tools natively must NOT receive the tool name list, descriptions,
// or parameter schemas as system-prompt text (they were double-sent on every
// turn), while the behavior contract stays. Fallback-only providers keep the
// full text catalog — for them the prompt IS the tool interface.
func TestToolPromptDedupForNativeProviders(t *testing.T) {
	build := func(p llm.Provider) string {
		agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, p, "test", 1, 1, nil)
		prompt, _, _ := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "check the repo")
		return prompt
	}

	native := build(&nativeToolsProvider{})
	if strings.Contains(native, "PARAM-SCHEMA-MARKER") || strings.Contains(native, "## Available Tools") {
		t.Fatal("native provider must not get tool schemas duplicated into the system prompt")
	}
	if strings.Contains(native, "The ONLY valid tool names are") {
		t.Fatal("native provider must not get the text tool-name list")
	}
	if !strings.Contains(native, "treat the error as diagnostic evidence") {
		t.Fatal("behavior contract must remain for native providers")
	}
	if !strings.Contains(native, "native tool-calling interface") {
		t.Fatal("native providers should get the short native-tools note")
	}

	text := build(&textOnlyProvider{})
	if !strings.Contains(text, "PARAM-SCHEMA-MARKER") || !strings.Contains(text, "## Available Tools") {
		t.Fatal("fallback provider must keep the full text tool catalog")
	}
	if !strings.Contains(text, "[TOOL:tool_name:") {
		t.Fatal("fallback provider must keep the fallback format instructions")
	}
}
