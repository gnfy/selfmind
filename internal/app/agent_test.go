package app

import (
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func TestModelSetupDiagnosticIncludesConfiguredProviderAndReason(t *testing.T) {
	cfg := &config.Config{
		Path:  "/tmp/selfmind-config.yaml",
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
	}
	cfg.Normalize()

	msg := modelSetupDiagnostic(cfg, fmt.Errorf("no credentials found for provider kimi-coding"))

	for _, want := range []string{
		"provider=kimi-coding model=kimi-for-coding",
		"Config: /tmp/selfmind-config.yaml",
		"Reason: no credentials found for provider kimi-coding",
		"selfmind model",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic missing %q:\n%s", want, msg)
		}
	}
}

func TestBuildResponsesProviderPreservesTokenGetter(t *testing.T) {
	provider := buildProviderFromRuntime(modelruntime.Runtime{
		Provider:    "codex-cli",
		Protocol:    modelruntime.ProtocolResponses,
		BaseURL:     "https://chatgpt.example.test/backend-api/codex",
		APIKey:      "old-token",
		Model:       "gpt-5.5",
		Quirks:      modelruntime.ProviderQuirks{ResponsesStoreFalse: true, ResponsesRequireStream: true},
		TokenGetter: func() string { return "fresh-token" },
		TokenRefresher: func() string {
			return "refreshed-token"
		},
	})
	responses, ok := provider.(*llm.ResponsesAdapter)
	if !ok {
		t.Fatalf("provider = %T, want *llm.ResponsesAdapter", provider)
	}
	if responses.KeyGetter == nil {
		t.Fatal("KeyGetter = nil")
	}
	if got := responses.KeyGetter(); got != "fresh-token" {
		t.Fatalf("KeyGetter() = %q", got)
	}
	if responses.TokenRefresher == nil {
		t.Fatal("TokenRefresher = nil")
	}
	if got := responses.TokenRefresher(); got != "refreshed-token" {
		t.Fatalf("TokenRefresher() = %q", got)
	}
	if responses.Store == nil || *responses.Store {
		t.Fatalf("Store = %v, want pointer to false", responses.Store)
	}
	if !responses.RequireStream {
		t.Fatal("RequireStream = false, want true")
	}
}

// Compaction runs at the summarizer role's reasoning when the role names one,
// and at the Background route's otherwise.
func TestSummarizerReasoningFollowsTheRoleThenBackground(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model", Reasoning: "xhigh"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model", Reasoning: "high"}
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Normalize()
	if got := summarizerReasoning(cfg); got != "high" {
		t.Fatalf("summarizer reasoning = %q, want the Background route's high", got)
	}
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		string(llm.RoleSummarizer): {Provider: "openai", Model: "summary-model", APIKey: "test-key", Reasoning: "low"},
	}
	cfg.Normalize()
	if got := summarizerReasoning(cfg); got != "low" {
		t.Fatalf("summarizer reasoning = %q, want the explicit role's low", got)
	}
}

func TestSummarizerOutputLimitUsesResolvedRoleCapacity(t *testing.T) {
	cfg := &config.Config{Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
		string(llm.RoleSummarizer): {MaxTokens: 3072},
	}}}
	if got := summarizerOutputLimit(cfg); got != 3072 {
		t.Fatalf("summarizer output limit = %d, want 3072", got)
	}
}
