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
	Provider           string
	DisplayName        string
	Model              string
	Protocol           string
	BaseURL            string
	APIKey             string
	CredentialSource   string
	AuthType           string
	Headers            map[string]string
	ExtraBody          map[string]interface{}
	ExtraQuery         map[string]interface{}
	ContextLength      int
	ContextSource      string
	MaxTokens          int
	ReasoningEffort    string
	DefaultReasoning   string
	ReasoningLevels    []string
	Thinking           map[string]interface{}
	ServiceTier        string
	DefaultServiceTier string
	ServiceTiers       []string
	CapabilitySource   string
	Quirks             ProviderQuirks
	TokenGetter        func() string
	TokenRefresher     func() string
}

// Selection carries per-command or per-role overrides. Empty fields mean the
// resolver should fall back to config, provider profiles, or discovered auth.
type Selection struct {
	Provider        string
	Model           string
	BaseURL         string
	Protocol        string
	APIKey          string
	Headers         map[string]string
	ExtraHeaders    map[string]string
	ExtraBody       map[string]interface{}
	ExtraQuery      map[string]interface{}
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
	providerExplicit := strings.TrimSpace(selection.Provider) != ""
	modelExplicit := strings.TrimSpace(selection.Model) != ""
	primary := r.cfg.EffectivePrimary()
	if !providerExplicit {
		selection.Provider = primary.Provider
	}
	if !modelExplicit && !providerExplicit {
		selection.Model = primary.Model
		if strings.TrimSpace(selection.ReasoningEffort) == "" {
			selection.ReasoningEffort = primary.Reasoning
		}
		if strings.TrimSpace(selection.ServiceTier) == "" {
			selection.ServiceTier = primary.ServiceTier
		}
		if selection.ContextLength <= 0 {
			selection.ContextLength = primary.ContextLength
		}
	}
	providerName := strings.TrimSpace(selection.Provider)
	modelName := strings.TrimSpace(selection.Model)
	if providerName == "" {
		return r.resolveAuto(selection, modelName)
	}
	return r.resolveNamed(providerName, selection, modelName)
}

func (r *Resolver) resolveAuto(selection Selection, modelName string) (Runtime, error) {
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
		rt, err := r.resolveNamed(candidate, selection, modelName)
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
	protocol := NormalizeProtocol(firstNonEmpty(selection.Protocol, endpoint.Protocol, profile.Protocol))
	model := firstNonEmpty(modelName, endpoint.Model, firstModel(profile.FallbackModels))
	cred := r.resolveCredential(profile, endpoint, selection.APIKey)
	if cred.Token == "" && profile.AuthType != AuthNone {
		return Runtime{}, fmt.Errorf("no credentials found for provider %s", profile.ID)
	}
	baseURL, protocol = resolveProviderTransport(profile, baseURL, protocol, endpoint, selection, cred)
	// Global config headers sit at the BOTTOM: a user-wide User-Agent must
	// never crush a built-in compatibility header (e.g. kimi-coding's).
	headers := mergeHeaders(r.cfg.Model.Headers, r.cfg.Model.ExtraHeaders, profile.Headers,
		endpoint.Headers, endpoint.ExtraHeaders, selection.Headers, selection.ExtraHeaders)
	if cred.AccountID != "" {
		if headers == nil {
			headers = map[string]string{}
		}
		// Required by the ChatGPT Codex backend; missing it can cause EOF.
		headers["chatgpt-account-id"] = cred.AccountID
	}
	descriptor, _ := DiscoverModelDescriptor(profile.ID, model)
	contextLength, contextSource := resolvedContextLength(
		selection.ContextLength,
		endpoint.ContextLength,
		profile.ContextLength,
		descriptor.ContextWindow,
		KnownContextLength(profile.ID, model),
	)
	resolvedQuirks := mergeProviderQuirks(profile.Quirks, quirksFromConfig(endpoint.Quirks), selection.Quirks)
	if err := ValidateProviderQuirks(resolvedQuirks); err != nil {
		return Runtime{}, fmt.Errorf("provider %s quirks: %w", profile.ID, err)
	}
	return Runtime{
		Provider: profile.ID, DisplayName: firstNonEmpty(profile.DisplayName, profile.ID),
		Model: model, Protocol: protocol, BaseURL: baseURL,
		APIKey: cred.Token, CredentialSource: cred.Source, AuthType: profile.AuthType,
		Headers:            headers,
		ExtraBody:          mergeExtraMaps(endpoint.ExtraBody, selection.ExtraBody),
		ExtraQuery:         mergeExtraMaps(endpoint.ExtraQuery, selection.ExtraQuery),
		ContextLength:      contextLength,
		ContextSource:      contextSource,
		MaxTokens:          firstPositive(selection.MaxTokens, endpoint.MaxTokens, profile.MaxTokens),
		ReasoningEffort:    firstNonEmpty(selection.ReasoningEffort, endpoint.ReasoningEffort, profile.ReasoningEffort),
		DefaultReasoning:   descriptor.DefaultReasoning,
		ReasoningLevels:    append([]string(nil), descriptor.SupportedReasoning...),
		Thinking:           firstThinking(selection.Thinking, endpoint.Thinking, profile.Thinking),
		ServiceTier:        firstNonEmpty(selection.ServiceTier, endpoint.ServiceTier, profile.ServiceTier),
		DefaultServiceTier: descriptor.DefaultServiceTier,
		ServiceTiers:       append([]string(nil), descriptor.SupportedServiceTiers...),
		CapabilitySource:   descriptor.CapabilitySource,
		Quirks:             resolvedQuirks,
		TokenGetter:        cred.Getter,
		TokenRefresher:     cred.Refresher,
	}, nil
}

func (r *Resolver) resolveCustom(providerName string, selection Selection, modelName string) (Runtime, error) {
	name := strings.TrimSpace(providerName)
	if strings.HasPrefix(strings.ToLower(name), "custom:") {
		name = strings.TrimSpace(name[len("custom:"):])
	}
	for _, cp := range r.cfg.Providers.Custom {
		if strings.EqualFold(cp.Name, name) || strings.EqualFold("custom:"+cp.Name, providerName) {
			protocol := NormalizeProtocol(firstNonEmpty(selection.Protocol, cp.Protocol, ProtocolOpenAICompatible))
			model := firstNonEmpty(modelName, cp.Model, selection.Model)
			contextLength, contextSource := resolvedCustomContextLength(
				selection.ContextLength,
				customModelContextLength(cp, model),
				KnownContextLength("custom:"+cp.Name, model),
			)
			resolvedQuirks := mergeProviderQuirks(defaultQuirksForProtocol(protocol), selection.Quirks)
			if err := ValidateProviderQuirks(resolvedQuirks); err != nil {
				return Runtime{}, fmt.Errorf("provider custom:%s quirks: %w", cp.Name, err)
			}
			return Runtime{
				Provider: "custom:" + cp.Name, DisplayName: cp.Name,
				Model:            model,
				Protocol:         protocol,
				BaseURL:          firstNonEmpty(selection.BaseURL, cp.BaseURL),
				APIKey:           firstNonEmpty(selection.APIKey, cp.APIKey),
				CredentialSource: "config:custom",
				AuthType:         AuthAPIKey,
				Headers:          mergeHeaders(r.cfg.Model.Headers, r.cfg.Model.ExtraHeaders, selection.Headers, selection.ExtraHeaders),
				ExtraBody:        mergeExtraMaps(selection.ExtraBody),
				ExtraQuery:       mergeExtraMaps(selection.ExtraQuery),
				ContextLength:    contextLength,
				ContextSource:    contextSource,
				MaxTokens:        selection.MaxTokens,
				ReasoningEffort:  selection.ReasoningEffort,
				Thinking:         selection.Thinking,
				ServiceTier:      selection.ServiceTier,
				Quirks:           resolvedQuirks,
			}, nil
		}
	}
	return Runtime{}, fmt.Errorf("unknown provider: %s", providerName)
}

func resolvedContextLength(values ...int) (int, string) {
	sources := []string{"explicit config", "provider profile", "built-in profile", "provider model metadata", "built-in fallback"}
	for i, value := range values {
		if value <= 0 {
			continue
		}
		source := "resolved"
		if i < len(sources) {
			source = sources[i]
		}
		return value, source
	}
	return 0, "unknown"
}

func resolvedCustomContextLength(explicit, modelMetadata, fallback int) (int, string) {
	switch {
	case explicit > 0:
		return explicit, "explicit config"
	case modelMetadata > 0:
		return modelMetadata, "custom model metadata"
	case fallback > 0:
		return fallback, "built-in fallback"
	default:
		return 0, "unknown"
	}
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
		userProtocol := strings.TrimSpace(selection.Protocol) != "" || strings.TrimSpace(endpoint.Protocol) != ""
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

// HeaderOrigins labels each resolved header with the layer that supplied its
// final value, mirroring the merge order in the builtin resolve path
// (model.headers < built-in profile < provider config < credential).
// Display-only: `model check` uses it so a user who just added an emergency
// override can see it actually took effect.
func (r *Resolver) HeaderOrigins(provider string, headers map[string]string) map[string]string {
	endpoint := r.endpointFor(provider)
	var profileHeaders map[string]string
	if profile, ok := r.registry.Resolve(provider); ok {
		profileHeaders = profile.Headers
	}
	origins := make(map[string]string, len(headers))
	for key := range headers {
		switch {
		case strings.EqualFold(key, "chatgpt-account-id"):
			origins[key] = "credential"
		case headerLayerHas(endpoint.ExtraHeaders, key):
			origins[key] = "provider config extra_headers"
		case headerLayerHas(endpoint.Headers, key):
			origins[key] = "provider config"
		case headerLayerHas(profileHeaders, key):
			origins[key] = "built-in profile"
		case headerLayerHas(r.cfg.Model.ExtraHeaders, key):
			origins[key] = "model.extra_headers"
		case headerLayerHas(r.cfg.Model.Headers, key):
			origins[key] = "model.headers"
		default:
			origins[key] = "selection"
		}
	}
	return origins
}

func headerLayerHas(layer map[string]string, key string) bool {
	for k := range layer {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return true
		}
	}
	return false
}

func mergeHeaders(headerSets ...map[string]string) map[string]string {
	out := map[string]string{}
	canonical := map[string]string{}
	for _, headers := range headerSets {
		for key, value := range headers {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			lower := strings.ToLower(key)
			if previous := canonical[lower]; previous != "" && previous != key {
				delete(out, previous)
			}
			canonical[lower] = key
			out[key] = strings.TrimSpace(value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeExtraMaps(layers ...map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, layer := range layers {
		for key, value := range layer {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if current, ok := out[key].(map[string]interface{}); ok {
				if incoming, ok := value.(map[string]interface{}); ok {
					out[key] = mergeExtraMaps(current, incoming)
					continue
				}
			}
			out[key] = cloneExtraValue(value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneExtraValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return mergeExtraMaps(typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = cloneExtraValue(item)
		}
		return out
	default:
		return value
	}
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
	out := ProviderQuirks{
		AuthHeader:        strings.TrimSpace(q.AuthHeader),
		ToolSchema:        strings.TrimSpace(q.ToolSchema),
		SystemMessageMode: strings.TrimSpace(q.SystemMessageMode),
		ThinkingMode:      strings.TrimSpace(q.ThinkingMode),
		UserIdentityField: strings.TrimSpace(q.UserIdentityField),
		UserAgent:         strings.TrimSpace(q.UserAgent),
		HTTPVersion:       strings.TrimSpace(q.HTTPVersion),
	}
	if q.ResponsesStoreFalse != nil {
		out.ResponsesStoreFalse = *q.ResponsesStoreFalse
		out.ResponsesStoreFalseSet = true
	}
	if q.ResponsesRequireStream != nil {
		out.ResponsesRequireStream = *q.ResponsesRequireStream
		out.ResponsesRequireStreamSet = true
	}
	if q.PromptCache != nil {
		out.PromptCache = *q.PromptCache
		out.PromptCacheSet = true
	}
	return out
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
		if strings.TrimSpace(overlay.UserIdentityField) != "" {
			out.UserIdentityField = strings.TrimSpace(overlay.UserIdentityField)
		}
		if strings.TrimSpace(overlay.UserAgent) != "" {
			out.UserAgent = strings.TrimSpace(overlay.UserAgent)
		}
		if strings.TrimSpace(overlay.HTTPVersion) != "" && !strings.EqualFold(strings.TrimSpace(overlay.HTTPVersion), "auto") {
			out.HTTPVersion = strings.TrimSpace(overlay.HTTPVersion)
			out.DisableHTTP2 = strings.EqualFold(out.HTTPVersion, "http1")
		} else if overlay.DisableHTTP2 {
			out.DisableHTTP2 = true
			out.HTTPVersion = "http1"
		}
		if overlay.PromptCacheSet {
			out.PromptCache = overlay.PromptCache
		} else if overlay.PromptCache {
			out.PromptCache = true
		}
		if overlay.ResponsesStoreFalseSet {
			out.ResponsesStoreFalse = overlay.ResponsesStoreFalse
		} else if overlay.ResponsesStoreFalse {
			out.ResponsesStoreFalse = true
		}
		if overlay.ResponsesRequireStreamSet {
			out.ResponsesRequireStream = overlay.ResponsesRequireStream
		} else if overlay.ResponsesRequireStream {
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
	q.UserIdentityField = strings.ToLower(strings.TrimSpace(q.UserIdentityField))
	q.UserAgent = strings.TrimSpace(q.UserAgent)
	q.HTTPVersion = strings.ToLower(strings.TrimSpace(q.HTTPVersion))
	if q.HTTPVersion == "" {
		if q.DisableHTTP2 {
			q.HTTPVersion = "http1"
		} else {
			q.HTTPVersion = "auto"
		}
	}
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
