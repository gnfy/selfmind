package modelruntime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"selfmind/internal/platform/config"
)

// Runtime is the fully resolved provider choice that the app layer can turn
// into an llm.Provider without knowing where credentials or defaults came from.
type Runtime struct {
	Provider         string
	DisplayName      string
	Model            string
	Protocol         string
	BaseURL          string
	APIKey           string
	CredentialSource string
	AuthType         string
}

// Selection carries per-command or per-role overrides. Empty fields mean the
// resolver should fall back to config, provider profiles, or discovered auth.
type Selection struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// Resolver owns provider lookup, profile overrides, and credential precedence.
// LLM adapters should stay protocol-focused and not repeat this discovery logic.
type Resolver struct {
	cfg      *config.Config
	registry *Registry
	store    *CredentialStore
	external ExternalCredentialResolver
}

func NewResolver(cfg *config.Config) *Resolver {
	if cfg == nil {
		cfg = &config.Config{}
	}
	r := &Resolver{
		cfg:      cfg,
		registry: NewRegistry(),
		store:    NewCredentialStore(cfg.Auth.CredentialsFile),
	}
	for id, endpoint := range cfg.ProviderProfiles {
		id = NormalizeProviderID(id)
		if id == "" {
			continue
		}
		// Built-ins keep their richer aliases, model-list behavior, and external
		// auth hints. YAML profiles only create new providers or override endpoint
		// fields later through endpointFor.
		if _, ok := r.registry.Resolve(id); ok {
			continue
		}
		r.registry.Register(ProviderProfile{
			ID: id, DisplayName: id,
			Protocol:  firstNonEmpty(endpoint.Protocol, ProtocolOpenAICompatible),
			AuthType:  AuthAPIKey,
			BaseURL:   endpoint.BaseURL,
			ModelList: ModelListOpenAICompatible,
		})
	}
	return r
}

func (r *Resolver) Registry() *Registry {
	return r.registry
}

// Resolve applies the same provider precedence for TUI, gateway, model roles,
// and CLI model commands so future SaaS policy can swap config sources safely.
func (r *Resolver) Resolve(ctx context.Context, selection Selection) (Runtime, error) {
	_ = ctx
	providerName := firstNonEmpty(selection.Provider, r.cfg.EffectiveProvider())
	modelName := firstNonEmpty(selection.Model, r.cfg.EffectiveModel())
	if providerName == "" {
		return r.resolveAuto(modelName)
	}
	return r.resolveNamed(providerName, selection, modelName)
}

func (r *Resolver) resolveAuto(modelName string) (Runtime, error) {
	// Auto mode probes likely local choices in a stable order and only succeeds
	// when usable credentials were actually found.
	candidates := []string{
		r.cfg.EffectiveProvider(),
		"anthropic",
		"google",
		"openai",
		"claude-code",
		"gemini-cli",
		"qwen-cli",
		"codex-cli",
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		rt, err := r.resolveNamed(candidate, Selection{}, modelName)
		if err == nil && rt.APIKey != "" {
			return rt, nil
		}
	}
	return Runtime{}, fmt.Errorf("no model provider credentials configured")
}

func (r *Resolver) resolveNamed(providerName string, selection Selection, modelName string) (Runtime, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(providerName)), "custom:") {
		return r.resolveCustom(providerName, selection, modelName)
	}

	profile, ok := r.registry.Resolve(providerName)
	if !ok {
		return r.resolveCustom(providerName, selection, modelName)
	}
	endpoint := r.endpointFor(profile.ID)
	// Explicit selection wins, then YAML endpoint overrides, env base URL, and
	// finally the built-in profile default.
	baseURL := firstNonEmpty(selection.BaseURL, endpoint.BaseURL, envValue(profile.BaseURLEnvVar), profile.BaseURL)
	protocol := NormalizeProtocol(firstNonEmpty(endpoint.Protocol, profile.Protocol))
	model := firstNonEmpty(modelName, endpoint.Model, firstModel(profile.FallbackModels))
	cred := r.resolveCredential(profile, endpoint, selection.APIKey)
	if cred.Token == "" && profile.AuthType != AuthNone {
		return Runtime{}, fmt.Errorf("no credentials found for provider %s", profile.ID)
	}
	return Runtime{
		Provider: profile.ID, DisplayName: firstNonEmpty(profile.DisplayName, profile.ID),
		Model: model, Protocol: protocol, BaseURL: baseURL,
		APIKey: cred.Token, CredentialSource: cred.Source, AuthType: profile.AuthType,
	}, nil
}

func (r *Resolver) resolveCustom(providerName string, selection Selection, modelName string) (Runtime, error) {
	name := strings.TrimSpace(providerName)
	if strings.HasPrefix(strings.ToLower(name), "custom:") {
		name = strings.TrimSpace(name[len("custom:"):])
	}
	for _, cp := range r.cfg.Providers.Custom {
		if strings.EqualFold(cp.Name, name) || strings.EqualFold("custom:"+cp.Name, providerName) {
			protocol := NormalizeProtocol(firstNonEmpty(cp.Protocol, ProtocolOpenAICompatible))
			return Runtime{
				Provider: "custom:" + cp.Name, DisplayName: cp.Name,
				Model:            firstNonEmpty(modelName, cp.Model, selection.Model),
				Protocol:         protocol,
				BaseURL:          firstNonEmpty(selection.BaseURL, cp.BaseURL),
				APIKey:           firstNonEmpty(selection.APIKey, cp.APIKey),
				CredentialSource: "config:custom",
				AuthType:         AuthAPIKey,
			}, nil
		}
	}
	return Runtime{}, fmt.Errorf("unknown provider: %s", providerName)
}

func (r *Resolver) endpointFor(provider string) config.ProviderEndpoint {
	id := NormalizeProviderID(provider)
	switch id {
	case "openai", "openai-api":
		return r.cfg.Providers.OpenAI
	case "anthropic":
		return r.cfg.Providers.Anthropic
	case "google", "gemini":
		return r.cfg.Providers.Google
	}
	if ep, ok := r.cfg.ProviderProfiles[id]; ok {
		return ep
	}
	// Legacy flat keys are still mapped here so old config files keep working
	// while new saves move toward nested providers/provider_profiles.
	switch id {
	case "claude-code":
		return r.cfg.Providers.Anthropic
	case "gemini-cli", "google-gemini-cli":
		return r.cfg.Providers.Google
	case "openrouter":
		return config.ProviderEndpoint{APIKey: r.cfg.Providers.OpenRouterAPIKey}
	case "minimax":
		return config.ProviderEndpoint{APIKey: r.cfg.Providers.MiniMaxAPIKey}
	default:
		return config.ProviderEndpoint{}
	}
}

func (r *Resolver) resolveCredential(profile ProviderProfile, endpoint config.ProviderEndpoint, explicit string) Credential {
	// Credential precedence is intentionally narrow and deterministic: runtime
	// override, YAML, SelfMind credential store, env vars, then external CLI reuse.
	if token := strings.TrimSpace(explicit); token != "" {
		return Credential{Token: token, Source: "explicit"}
	}
	if token := strings.TrimSpace(endpoint.APIKey); token != "" {
		return Credential{Token: token, Source: "config"}
	}
	if cred := r.store.Resolve(profile.ID); cred.Token != "" {
		return cred
	}
	for _, env := range profile.APIKeyEnvVars {
		if token := strings.TrimSpace(os.Getenv(env)); token != "" {
			return Credential{Token: token, Source: "env:" + env}
		}
	}
	if source := firstNonEmpty(profile.ExternalSource, externalSourceFor(profile.ID)); source != "" {
		if cred := r.external.Resolve(source); cred.Token != "" {
			return cred
		}
	}
	return Credential{}
}

func externalSourceFor(provider string) string {
	switch NormalizeProviderID(provider) {
	case "anthropic":
		return "claude-code"
	default:
		return ""
	}
}

func envValue(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(name))
}

func firstModel(models []string) string {
	if len(models) == 0 {
		return ""
	}
	return strings.TrimSpace(models[0])
}
