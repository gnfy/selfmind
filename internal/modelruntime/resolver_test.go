package modelruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/platform/config"
)

func TestResolverReusesCodexCLIAuthFromSelfMindStore(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"providers":{"codex-cli":{"access_token":"codex-token"}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "codex-cli", Default: "gpt-5.5"},
		Auth:  config.AuthConfig{CredentialsFile: authPath},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"codex-cli": {BaseURL: "https://chatgpt.example.test/backend-api/codex"},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.Provider != "codex-cli" {
		t.Fatalf("provider = %q, want codex-cli", rt.Provider)
	}
	if rt.Protocol != ProtocolResponses {
		t.Fatalf("protocol = %q, want %q", rt.Protocol, ProtocolResponses)
	}
	if rt.APIKey != "codex-token" {
		t.Fatalf("token = %q, want codex-token", rt.APIKey)
	}
	if rt.BaseURL != "https://chatgpt.example.test/backend-api/codex" {
		t.Fatalf("baseURL = %q", rt.BaseURL)
	}
}

func TestResolverKeepsLegacyOpenRouterKeyCompatible(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "openrouter", Default: "anthropic/claude-3.5-sonnet"},
		Providers: config.ProvidersConfig{
			OpenRouterAPIKey: "openrouter-token",
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.Provider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", rt.Provider)
	}
	if rt.APIKey != "openrouter-token" {
		t.Fatalf("token = %q, want openrouter-token", rt.APIKey)
	}
}

func TestResolverAllowsProviderProfileProtocolOverride(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {
				BaseURL:  "https://api.kimi.com/coding",
				APIKey:   "kimi-token",
				Protocol: ProtocolAnthropic,
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.Protocol != ProtocolAnthropic {
		t.Fatalf("protocol = %q, want %q", rt.Protocol, ProtocolAnthropic)
	}
	if rt.BaseURL != "https://api.kimi.com/coding" {
		t.Fatalf("baseURL = %q", rt.BaseURL)
	}
}
