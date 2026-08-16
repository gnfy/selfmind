package modelruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/buildinfo"
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
	if !rt.Quirks.ResponsesStoreFalse {
		t.Fatalf("ResponsesStoreFalse = false, want true for codex-cli")
	}
	if !rt.Quirks.ResponsesRequireStream {
		t.Fatalf("ResponsesRequireStream = false, want true for codex-cli")
	}
	if rt.Headers["originator"] != "codex_cli_rs" {
		t.Fatalf("originator header = %q, want codex_cli_rs", rt.Headers["originator"])
	}
	if rt.Headers["OpenAI-Beta"] != "responses=experimental" {
		t.Fatalf("OpenAI-Beta header = %q", rt.Headers["OpenAI-Beta"])
	}
}

func TestResolverUsesPrimarySelectionAndCodexCapabilities(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{
  "models": [{
    "slug": "gpt-5.6-sol",
    "context_window": 272000,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": [{"effort":"low"},{"effort":"xhigh"}]
  }]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{
			Provider:  "codex-cli",
			Model:     "gpt-5.6-sol",
			Reasoning: "xhigh",
		}},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"codex-cli": {APIKey: "test-token"},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Model != "gpt-5.6-sol" || rt.ReasoningEffort != "xhigh" {
		t.Fatalf("runtime selection = %+v", rt)
	}
	if rt.ContextLength != 272000 || rt.ContextSource != "provider model metadata" {
		t.Fatalf("context = %d from %q", rt.ContextLength, rt.ContextSource)
	}
	if rt.DefaultReasoning != "medium" || len(rt.ReasoningLevels) != 2 {
		t.Fatalf("capabilities = %+v", rt)
	}
}

func TestResolverRoleOptionInheritsPrimaryWithoutLosingOverride(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{
			Provider:  "openai",
			Model:     "gpt-primary",
			Reasoning: "medium",
		}},
		Providers: config.ProvidersConfig{
			OpenAI: config.ProviderEndpoint{APIKey: "test-token"},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider != "openai" || rt.Model != "gpt-primary" || rt.ReasoningEffort != "high" {
		t.Fatalf("role option did not inherit primary selection: %+v", rt)
	}
}

func TestResolverExplicitRoleProviderUsesItsOwnModelDefault(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{
			Provider:  "openai",
			Model:     "gpt-primary",
			Reasoning: "high",
		}},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {APIKey: "test-token"},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{Provider: "kimi-coding"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider != "kimi-coding" || rt.Model != "kimi-for-coding" {
		t.Fatalf("explicit role provider inherited an incompatible primary model: %+v", rt)
	}
	if rt.ReasoningEffort != "" {
		t.Fatalf("explicit role provider inherited primary reasoning: %q", rt.ReasoningEffort)
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
	if rt.Quirks.PromptCache {
		t.Fatal("direct Kimi Coding must not receive unverified cache_control markers")
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
	if rt.ReasoningEffort != "" {
		t.Fatalf("reasoning effort should use the provider default, got %q", rt.ReasoningEffort)
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
	if !rt.Quirks.PromptCache {
		t.Fatal("MiniMax Anthropic endpoint should enable documented prompt caching")
	}
}

func TestBuiltinPromptCachePolicy(t *testing.T) {
	registry := NewRegistry()
	for _, provider := range []string{"anthropic", "claude-code", "minimax", "minimax-cn", "minimax-oauth"} {
		profile, ok := registry.Resolve(provider)
		if !ok {
			t.Fatalf("builtin provider %q not found", provider)
		}
		if !profile.Quirks.PromptCache {
			t.Errorf("provider %q should enable its documented Anthropic prompt-cache path", provider)
		}
	}

	kimi, ok := registry.Resolve("kimi-coding")
	if !ok {
		t.Fatal("builtin provider kimi-coding not found")
	}
	if kimi.Quirks.PromptCache {
		t.Fatal("direct Kimi Coding must keep unverified cache_control markers disabled")
	}
}

func TestProviderConfigCanDisableBuiltinPromptCacheAndForceHTTP2(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{Provider: "anthropic", Model: "claude-test"}},
		Providers: config.ProvidersConfig{
			Anthropic: config.ProviderEndpoint{
				APIKey: "test-key",
				Quirks: config.ProviderQuirks{PromptCache: &disabled, HTTPVersion: "http2"},
			},
		},
	}
	cfg.Normalize()
	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Quirks.PromptCache {
		t.Fatal("explicit prompt_cache: false did not override the built-in Anthropic profile")
	}
	if rt.Quirks.DisableHTTP2 || rt.Quirks.HTTPVersion != "http2" {
		t.Fatalf("HTTP override = version=%q disable_http2=%t", rt.Quirks.HTTPVersion, rt.Quirks.DisableHTTP2)
	}
}

func TestResolverRejectsUnknownQuirkValues(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-test"}},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"deepseek": {APIKey: "test-key", Quirks: config.ProviderQuirks{HTTPVersion: "http3"}},
		},
	}
	cfg.Normalize()
	if _, err := NewResolver(cfg).Resolve(context.Background(), Selection{}); err == nil || !strings.Contains(err.Error(), "unsupported http_version") {
		t.Fatalf("Resolve error = %v, want unsupported http_version", err)
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

// Global model.headers is the lowest layer: it reaches every provider but a
// built-in compatibility header (kimi's User-Agent) must crush it, and a
// provider_profiles header must crush both.
func TestGlobalModelHeadersLayering(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Provider: "kimi-coding",
			Default:  "kimi-for-coding",
			Headers: map[string]string{
				"User-Agent":   "my-org/1.0",
				"X-Org-Header": "org-value",
				"X-Overridden": "from-global",
			},
		},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {
				APIKey:  "sk-kimi-test",
				Headers: map[string]string{"X-Overridden": "from-provider"},
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := rt.Headers["User-Agent"]; got != "claude-code/0.1.0" {
		t.Fatalf("built-in compatibility UA must beat global headers, got %q", got)
	}
	if got := rt.Headers["X-Org-Header"]; got != "org-value" {
		t.Fatalf("global header should reach the request, got %q", got)
	}
	if got := rt.Headers["X-Overridden"]; got != "from-provider" {
		t.Fatalf("provider_profiles header must beat global, got %q", got)
	}

	origins := NewResolver(cfg).HeaderOrigins("kimi-coding", rt.Headers)
	if origins["User-Agent"] != "built-in profile" {
		t.Fatalf("User-Agent origin = %q", origins["User-Agent"])
	}
	if origins["X-Org-Header"] != "model.headers" {
		t.Fatalf("X-Org-Header origin = %q", origins["X-Org-Header"])
	}
	if origins["X-Overridden"] != "provider config" {
		t.Fatalf("X-Overridden origin = %q", origins["X-Overridden"])
	}
}

// The emergency escape hatch: a provider_profiles header can override a
// built-in compatibility value without a code change.
func TestProviderProfileHeaderOverridesBuiltin(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {
				APIKey:  "sk-kimi-test",
				Headers: map[string]string{"User-Agent": "claude-code/0.2.0"},
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := rt.Headers["User-Agent"]; got != "claude-code/0.2.0" {
		t.Fatalf("provider_profiles must override the built-in header, got %q", got)
	}
}

func TestProviderRequestExtrasMergeWithSelectionOverrides(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "deepseek", Default: "deepseek-v4-flash"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"deepseek": {
				APIKey:       "sk-test",
				Protocol:     "openai_compatible",
				ExtraHeaders: map[string]string{"X-Profile": "profile", "X-Override": "profile"},
				ExtraBody: map[string]interface{}{
					"user_id":  "profile-user",
					"metadata": map[string]interface{}{"profile": true, "override": "profile"},
				},
				ExtraQuery: map[string]interface{}{"api-version": "profile"},
			},
		},
	}
	cfg.Normalize()
	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{
		ExtraHeaders: map[string]string{"X-Override": "role"},
		ExtraBody: map[string]interface{}{
			"user_id":  "role-user",
			"metadata": map[string]interface{}{"override": "role"},
		},
		ExtraQuery: map[string]interface{}{"api-version": "role"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Headers["X-Profile"] != "profile" || rt.Headers["X-Override"] != "role" {
		t.Fatalf("headers = %#v", rt.Headers)
	}
	if rt.ExtraBody["user_id"] != "role-user" || rt.ExtraQuery["api-version"] != "role" {
		t.Fatalf("extras = body:%#v query:%#v", rt.ExtraBody, rt.ExtraQuery)
	}
	metadata := rt.ExtraBody["metadata"].(map[string]interface{})
	if metadata["profile"] != true || metadata["override"] != "role" {
		t.Fatalf("nested metadata = %#v", metadata)
	}
}

// OpenRouter app attribution must survive the protocol the user picks. The
// headers used to live only in the OpenRouter adapter, so a profile declared
// as openai_chat (a supported and common choice) reached OpenRouter with no
// attribution at all. They now come from the built-in profile layer, which
// every protocol carries.
func TestOpenRouterAttributionHeadersSurviveProtocolChoice(t *testing.T) {
	for _, protocol := range []string{"", ProtocolOpenAIChat, ProtocolOpenAICompatible} {
		cfg := &config.Config{
			Model: config.ModelConfig{Provider: "openrouter", Default: "some/model"},
			ProviderProfiles: map[string]config.ProviderEndpoint{
				"openrouter": {APIKey: "sk-or-test", Protocol: protocol},
			},
		}
		cfg.Normalize()

		rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
		if err != nil {
			t.Fatalf("protocol %q: Resolve() error = %v", protocol, err)
		}
		if got := rt.Headers["HTTP-Referer"]; got != "https://github.com/gnfy/selfmind" {
			t.Fatalf("protocol %q: HTTP-Referer = %q", protocol, got)
		}
		if got := rt.Headers["X-Title"]; got != "SelfMind Agent" {
			t.Fatalf("protocol %q: X-Title = %q", protocol, got)
		}
		// The agent identifies the running build, not a pinned string: a
		// provider-side breakdown by version must stay truthful after upgrade.
		wantUA := "selfmind/" + strings.TrimPrefix(buildinfo.Version, "v")
		if got := rt.Headers["User-Agent"]; got != wantUA {
			t.Fatalf("protocol %q: User-Agent = %q, want %q", protocol, got, wantUA)
		}
		if origins := NewResolver(cfg).HeaderOrigins("openrouter", rt.Headers); origins["X-Title"] != "built-in profile" {
			t.Fatalf("protocol %q: X-Title origin = %q", protocol, origins["X-Title"])
		}
	}
}

// The attribution defaults stay overridable: a user running a fork or a proxy
// must be able to send their own app identity from yaml.
func TestOpenRouterAttributionHeadersAreOverridable(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "openrouter", Default: "some/model"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"openrouter": {
				APIKey:       "sk-or-test",
				ExtraHeaders: map[string]string{"X-Title": "My Fork", "User-Agent": "my-fork/2.0"},
			},
		},
	}
	cfg.Normalize()

	rt, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := rt.Headers["X-Title"]; got != "My Fork" {
		t.Fatalf("X-Title = %q, want the configured override", got)
	}
	if got := rt.Headers["User-Agent"]; got != "my-fork/2.0" {
		t.Fatalf("User-Agent = %q, want the configured override", got)
	}
	if got := rt.Headers["HTTP-Referer"]; got != "https://github.com/gnfy/selfmind" {
		t.Fatalf("un-overridden default must remain, got %q", got)
	}
	origins := NewResolver(cfg).HeaderOrigins("openrouter", rt.Headers)
	if origins["X-Title"] != "provider config extra_headers" {
		t.Fatalf("X-Title origin = %q", origins["X-Title"])
	}
}
