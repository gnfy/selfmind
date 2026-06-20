package modelruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestResolverKimiCodingDefaultsToAnthropicEndpoint(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {APIKey: "sk-kimi-test"},
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
	if rt.Model != "kimi-for-coding" {
		t.Fatalf("model = %q", rt.Model)
	}
	if rt.MaxTokens != 32000 {
		t.Fatalf("maxTokens = %d", rt.MaxTokens)
	}
	if rt.ContextLength != 262144 {
		t.Fatalf("contextLength = %d", rt.ContextLength)
	}
	if rt.Headers["User-Agent"] != "claude-code/0.1.0" {
		t.Fatalf("User-Agent header = %q", rt.Headers["User-Agent"])
	}
	if rt.Quirks.ToolSchema != ToolSchemaMoonshot {
		t.Fatalf("tool schema quirk = %q, want %q", rt.Quirks.ToolSchema, ToolSchemaMoonshot)
	}
	if rt.Quirks.ThinkingMode != ThinkingModeKimi {
		t.Fatalf("thinking mode quirk = %q, want %q", rt.Quirks.ThinkingMode, ThinkingModeKimi)
	}
	if rt.Quirks.UserAgent != "claude-code/0.1.0" {
		t.Fatalf("user agent quirk = %q", rt.Quirks.UserAgent)
	}
	if !rt.Quirks.DisableHTTP2 {
		t.Fatalf("DisableHTTP2 quirk = false, want true")
	}
}

func TestResolverKimiOpenAICompatibleUsesCodingV1Root(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {
				APIKey:   "sk-kimi-test",
				Protocol: ProtocolOpenAICompatible,
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.Protocol != ProtocolOpenAICompatible {
		t.Fatalf("protocol = %q", rt.Protocol)
	}
	if rt.BaseURL != "https://api.kimi.com/coding/v1" {
		t.Fatalf("baseURL = %q", rt.BaseURL)
	}
	if rt.ReasoningEffort != "medium" {
		t.Fatalf("reasoning effort = %q", rt.ReasoningEffort)
	}
	if rt.Thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", rt.Thinking)
	}
}

func TestResolverMiniMaxProfiles(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "minimax-cn", Default: "MiniMax-M3"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"minimax-cn": {APIKey: "mm-cn"},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.Protocol != ProtocolAnthropic {
		t.Fatalf("protocol = %q", rt.Protocol)
	}
	if rt.BaseURL != "https://api.minimaxi.com/anthropic" {
		t.Fatalf("baseURL = %q", rt.BaseURL)
	}
	if rt.MaxTokens != 32768 {
		t.Fatalf("maxTokens = %d", rt.MaxTokens)
	}
	if rt.ContextLength != 204800 {
		t.Fatalf("contextLength = %d", rt.ContextLength)
	}
	if rt.Quirks.AuthHeader != AuthHeaderBearer {
		t.Fatalf("auth header quirk = %q, want %q", rt.Quirks.AuthHeader, AuthHeaderBearer)
	}
	if rt.Quirks.ThinkingMode != ThinkingModeMiniMax {
		t.Fatalf("thinking mode quirk = %q, want %q", rt.Quirks.ThinkingMode, ThinkingModeMiniMax)
	}
}

func TestResolverContextLengthOverrides(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding", ContextLength: 131072},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {
				APIKey:        "sk-kimi-test",
				ContextLength: 262144,
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.ContextLength != 131072 {
		t.Fatalf("contextLength = %d, want model override", rt.ContextLength)
	}
}

func TestResolverCustomProviderContextLength(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "custom:ollama", Default: "llama3"},
		Providers: config.ProvidersConfig{
			Custom: []config.CustomProvider{{
				Name:     "ollama",
				BaseURL:  "http://localhost:11434/v1",
				APIKey:   "local",
				Protocol: ProtocolOpenAICompatible,
				Model:    "llama3",
				Models: map[string]config.CustomModelProperties{
					"llama3": {ContextLength: 8192},
				},
			}},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.ContextLength != 8192 {
		t.Fatalf("contextLength = %d, want custom model context", rt.ContextLength)
	}
}

func TestResolverProviderProfileQuirksOverride(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "custom-anthropic", Default: "custom-model"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"custom-anthropic": {
				BaseURL:  "https://provider.example.test/anthropic",
				APIKey:   "custom-token",
				Protocol: ProtocolAnthropic,
				Quirks: config.ProviderQuirks{
					AuthHeader:        AuthHeaderBearer,
					ToolSchema:        ToolSchemaMoonshot,
					SystemMessageMode: SystemMessageTopLevel,
					ThinkingMode:      ThinkingModeOmit,
					UserAgent:         "selfmind-test/1.0",
				},
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.Provider != "custom-anthropic" {
		t.Fatalf("provider = %q", rt.Provider)
	}
	if rt.Quirks.AuthHeader != AuthHeaderBearer {
		t.Fatalf("auth header quirk = %q", rt.Quirks.AuthHeader)
	}
	if rt.Quirks.ToolSchema != ToolSchemaMoonshot {
		t.Fatalf("tool schema quirk = %q", rt.Quirks.ToolSchema)
	}
	if rt.Quirks.ThinkingMode != ThinkingModeOmit {
		t.Fatalf("thinking mode quirk = %q", rt.Quirks.ThinkingMode)
	}
	if rt.Quirks.UserAgent != "selfmind-test/1.0" {
		t.Fatalf("user agent quirk = %q", rt.Quirks.UserAgent)
	}
	if !rt.Quirks.SupportsTools || !rt.Quirks.SupportsStreaming {
		t.Fatalf("protocol defaults should preserve tool/stream support: %+v", rt.Quirks)
	}
}

func TestResolverMiniMaxOAuthUsesStoredTokenGetter(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	payload := `{"providers":{"minimax-oauth":{"access_token":"oauth-token","refresh_token":"refresh-token","expires_at":"` + expires + `","portal_base_url":"https://api.minimax.io","inference_base_url":"https://api.minimax.io/anthropic","region":"global"}}}`
	if err := os.WriteFile(authPath, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "minimax-oauth", Default: "MiniMax-M3"},
		Auth:  config.AuthConfig{CredentialsFile: authPath},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if rt.APIKey != "oauth-token" || rt.TokenGetter == nil {
		t.Fatalf("oauth credential not resolved: token=%q getter=%v", rt.APIKey, rt.TokenGetter != nil)
	}
	if got := rt.TokenGetter(); got != "oauth-token" {
		t.Fatalf("TokenGetter() = %q", got)
	}
}
