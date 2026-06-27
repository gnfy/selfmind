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
	Headers          map[string]string
	ContextLength    int
	MaxTokens        int
	ReasoningEffort  string
	Thinking         map[string]interface{}
	ServiceTier      string
	Quirks           ProviderQuirks
	TokenGetter      func() string
	TokenRefresher   func() string
}

// Selection carries per-command or per-role overrides. Empty fields mean the
// resolver should fall back to config, provider profiles, or discovered auth.
type Selection struct {
	Provider        string
	Model           string
	BaseURL         string
	APIKey          string
	Headers         map[string]string
	ContextLength   int
	MaxTokens       int
	ReasoningEffort string
	Thinking        map[string]interface{}
	ServiceTier     string
	Quirks          ProviderQuirks
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
			Quirks:    defaultQuirksForProtocol(firstNonEmpty(endpoint.Protocol, ProtocolOpenAICompatible)),
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
	baseURL, protocol = resolveProviderTransport(profile, baseURL, protocol, endpoint, selection, cred)
	headers := mergeHeaders(profile.Headers, endpoint.Headers, selection.Headers)
	if cred.AccountID != "" {
		if headers == nil {
			headers = map[string]string{}
		}
		// Required by the ChatGPT Codex backend; missing it can cause EOF.
		headers["chatgpt-account-id"] = cred.AccountID
	}
	return Runtime{
		Provider: profile.ID, DisplayName: firstNonEmpty(profile.DisplayName, profile.ID),
		Model: model, Protocol: protocol, BaseURL: baseURL,
		APIKey: cred.Token, CredentialSource: cred.Source, AuthType: profile.AuthType,
		Headers:         headers,
		ContextLength:   firstPositive(selection.ContextLength, r.cfg.Model.ContextLength, endpoint.ContextLength, profile.ContextLength, KnownContextLength(profile.ID, model)),
		MaxTokens:       firstPositive(selection.MaxTokens, endpoint.MaxTokens, profile.MaxTokens),
		ReasoningEffort: firstNonEmpty(selection.ReasoningEffort, endpoint.ReasoningEffort, profile.ReasoningEffort),
		Thinking:        firstThinking(selection.Thinking, endpoint.Thinking, profile.Thinking),
		ServiceTier:     firstNonEmpty(selection.ServiceTier, endpoint.ServiceTier, profile.ServiceTier),
		Quirks:          mergeProviderQuirks(profile.Quirks, quirksFromConfig(endpoint.Quirks), selection.Quirks),
		TokenGetter:     cred.Getter,
		TokenRefresher:  cred.Refresher,
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
				Headers:          mergeHeaders(selection.Headers),
				ContextLength:    firstPositive(selection.ContextLength, r.cfg.Model.ContextLength, customModelContextLength(cp, firstNonEmpty(modelName, cp.Model, selection.Model)), KnownContextLength("custom:"+cp.Name, firstNonEmpty(modelName, cp.Model, selection.Model))),
				MaxTokens:        selection.MaxTokens,
				ReasoningEffort:  selection.ReasoningEffort,
				Thinking:         selection.Thinking,
				ServiceTier:      selection.ServiceTier,
				Quirks:           mergeProviderQuirks(defaultQuirksForProtocol(protocol), selection.Quirks),
			}, nil
		}
	}
	return Runtime{}, fmt.Errorf("unknown provider: %s", providerName)
}

func customModelContextLength(cp config.CustomProvider, model string) int {
	model = strings.TrimSpace(model)
	if model == "" || len(cp.Models) == 0 {
		return 0
	}
	if props, ok := cp.Models[model]; ok {
		return props.ContextLength
	}
	for name, props := range cp.Models {
		if strings.EqualFold(strings.TrimSpace(name), model) {
			return props.ContextLength
		}
	}
	return 0
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
	case "minimax-cn", "minimax-oauth":
		return config.ProviderEndpoint{}
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
	if profile.AuthType == AuthMiniMaxOAuth {
		return r.store.ResolveMiniMaxOAuth()
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

func resolveProviderTransport(profile ProviderProfile, baseURL, protocol string, endpoint config.ProviderEndpoint, selection Selection, cred Credential) (string, string) {
	id := NormalizeProviderID(profile.ID)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if id == "kimi-coding" {
		userBaseURL := strings.TrimSpace(selection.BaseURL) != "" || strings.TrimSpace(endpoint.BaseURL) != "" || envValue(profile.BaseURLEnvVar) != ""
		userProtocol := strings.TrimSpace(endpoint.Protocol) != ""
		if !userBaseURL && strings.HasPrefix(strings.TrimSpace(cred.Token), "sk-kimi-") {
			baseURL = "https://api.kimi.com/coding"
		}
		if !userProtocol && strings.Contains(strings.ToLower(baseURL), "api.kimi.com/coding") {
			protocol = ProtocolAnthropic
		}
		if protocol == ProtocolAnthropic && strings.HasSuffix(strings.ToLower(baseURL), "/coding/v1") {
			baseURL = baseURL[:len(baseURL)-len("/v1")]
		}
		if protocol == ProtocolOpenAICompatible && strings.Contains(strings.ToLower(baseURL), "api.kimi.com/coding") && !strings.HasSuffix(strings.ToLower(baseURL), "/v1") {
			baseURL = strings.TrimRight(baseURL, "/") + "/v1"
		}
	}
	return baseURL, NormalizeProtocol(protocol)
}

func mergeHeaders(headerSets ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, headers := range headerSets {
		for key, value := range headers {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			out[key] = strings.TrimSpace(value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstThinking(values ...map[string]interface{}) map[string]interface{} {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func defaultQuirksForProtocol(protocol string) ProviderQuirks {
	switch NormalizeProtocol(protocol) {
	case ProtocolAnthropic:
		return anthropicQuirks()
	case ProtocolResponses, ProtocolOpenAIChat, ProtocolOpenAICompatible:
		return openAIQuirks()
	default:
		return ProviderQuirks{AuthHeader: AuthHeaderAuto}
	}
}

func quirksFromConfig(q config.ProviderQuirks) ProviderQuirks {
	return ProviderQuirks{
		AuthHeader:             strings.TrimSpace(q.AuthHeader),
		ToolSchema:             strings.TrimSpace(q.ToolSchema),
		SystemMessageMode:      strings.TrimSpace(q.SystemMessageMode),
		ThinkingMode:           strings.TrimSpace(q.ThinkingMode),
		UserAgent:              strings.TrimSpace(q.UserAgent),
		ResponsesStoreFalse:    q.ResponsesStoreFalse,
		ResponsesRequireStream: q.ResponsesRequireStream,
	}
}

func mergeProviderQuirks(base ProviderQuirks, overlays ...ProviderQuirks) ProviderQuirks {
	out := base
	for _, overlay := range overlays {
		if strings.TrimSpace(overlay.AuthHeader) != "" {
			out.AuthHeader = strings.TrimSpace(overlay.AuthHeader)
		}
		if strings.TrimSpace(overlay.ToolSchema) != "" {
			out.ToolSchema = strings.TrimSpace(overlay.ToolSchema)
		}
		if strings.TrimSpace(overlay.SystemMessageMode) != "" {
			out.SystemMessageMode = strings.TrimSpace(overlay.SystemMessageMode)
		}
		if strings.TrimSpace(overlay.ThinkingMode) != "" {
			out.ThinkingMode = strings.TrimSpace(overlay.ThinkingMode)
		}
		if strings.TrimSpace(overlay.UserAgent) != "" {
			out.UserAgent = strings.TrimSpace(overlay.UserAgent)
		}
		if overlay.DisableHTTP2 {
			out.DisableHTTP2 = true
		}
		if overlay.ResponsesStoreFalse {
			out.ResponsesStoreFalse = true
		}
		if overlay.ResponsesRequireStream {
			out.ResponsesRequireStream = true
		}
		if overlay.SupportsTools {
			out.SupportsTools = true
		}
		if overlay.SupportsStreaming {
			out.SupportsStreaming = true
		}
		if overlay.SupportsVision {
			out.SupportsVision = true
		}
	}
	return normalizeProviderQuirks(out)
}

func normalizeProviderQuirks(q ProviderQuirks) ProviderQuirks {
	q.AuthHeader = strings.ToLower(strings.TrimSpace(q.AuthHeader))
	q.ToolSchema = strings.ToLower(strings.TrimSpace(q.ToolSchema))
	q.SystemMessageMode = strings.ToLower(strings.TrimSpace(q.SystemMessageMode))
	q.ThinkingMode = strings.ToLower(strings.TrimSpace(q.ThinkingMode))
	q.UserAgent = strings.TrimSpace(q.UserAgent)
	return q
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
