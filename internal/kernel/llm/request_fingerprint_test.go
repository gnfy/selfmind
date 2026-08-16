package llm

import (
	"context"
	"testing"
)

func TestOpenAIRequestFingerprintSeparatesPrefixFromConversation(t *testing.T) {
	adapter := &OpenAIAdapter{Model: "deepseek-v4-flash"}
	base := ChatRequest{
		Messages: []Message{{Role: "system", Content: "stable rules"}, {Role: "user", Content: "first task"}},
		Tools:    []ToolDefinition{{Name: "read_file", Parameters: map[string]interface{}{"type": "object"}}},
	}
	first, ok := adapter.FingerprintRequest(context.Background(), base, true)
	if !ok || first.PrefixHash == "" || first.RequestHash == "" {
		t.Fatalf("fingerprint missing: %+v ok=%v", first, ok)
	}
	changedUser := base
	changedUser.Messages = []Message{{Role: "system", Content: "stable rules"}, {Role: "user", Content: "another task"}}
	second, _ := adapter.FingerprintRequest(context.Background(), changedUser, true)
	if first.PrefixHash != second.PrefixHash {
		t.Fatalf("volatile user text changed prefix: %s != %s", first.PrefixHash, second.PrefixHash)
	}
	if first.RequestHash == second.RequestHash {
		t.Fatal("full request hash ignored user text")
	}
	changedSystem := base
	changedSystem.Messages = []Message{{Role: "system", Content: "different stable rules"}, {Role: "user", Content: "first task"}}
	third, _ := adapter.FingerprintRequest(context.Background(), changedSystem, true)
	if first.PrefixHash == third.PrefixHash || first.Blocks["system"] == third.Blocks["system"] {
		t.Fatal("system change did not invalidate provider prefix")
	}
}

func TestProtocolAdaptersExposeRequestFingerprints(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{{Role: "system", Content: "stable"}, {Role: "user", Content: "hello"}},
		Tools:    []ToolDefinition{{Name: "read_file", Parameters: map[string]interface{}{"type": "object"}}},
	}
	providers := []struct {
		name     string
		provider Provider
		protocol string
	}{
		{"responses", &ResponsesAdapter{Model: "gpt-test"}, "openai_responses"},
		{"anthropic", &AnthropicAdapter{Model: "claude-test", MaxTokens: 128}, "anthropic_messages"},
	}
	for _, test := range providers {
		t.Run(test.name, func(t *testing.T) {
			got, ok := FingerprintProviderRequest(context.Background(), test.provider, req, false)
			if !ok || got.Protocol != test.protocol || got.PrefixHash == "" || got.RequestHash == "" {
				t.Fatalf("fingerprint = %+v ok=%v", got, ok)
			}
		})
	}
}
