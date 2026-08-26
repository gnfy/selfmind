package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/promptassets"
	"selfmind/internal/tools"
)

// parseResilienceDuration parses a Go duration string, falling back to def when
// the value is empty or unparseable. Used for the LLM transport resilience
// knobs (llm_retry_base/llm_retry_cap/llm_stream_idle_timeout).
func parseResilienceDuration(value string, def time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		return def
	}
	if d, err := time.ParseDuration(strings.TrimSpace(value)); err == nil && d > 0 {
		return d
	}
	return def
}

// applyLLMResilience pushes the process-wide streaming idle-watchdog default
// from config into the llm transport layer. Per-attempt backoff/retry policy is
// applied per-agent via Agent.SetRetryPolicy.
func applyLLMResilience(cfg *config.Config) {
	llm.SetStreamIdleTimeout(parseResilienceDuration(cfg.Agent.LLMStreamIdleTimeout, llm.DefaultStreamIdle))
}

// mockProvider is used when no LLM provider can be resolved. It returns the
// setup diagnostic as a normal assistant response so the TUI can surface the
// actual configuration problem instead of failing silently.
type mockProvider struct {
	message string
}

const mockSetupGuide = `SelfMind has not been configured with an AI model yet.

Run:
  selfmind model

Or edit:
  ~/.selfmind/config.yaml

Example:
  model:
    provider: "openai"
    default: "gpt-4o"
  providers:
    openai:
      api_key: "${OPENAI_API_KEY}"
      base_url: "https://api.openai.com/v1"

SelfMind also supports Anthropic, Google, and custom OpenAI-compatible endpoints.`

func (m *mockProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return m.response(), nil
}

func (m *mockProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: m.response()}, nil
}

func (m *mockProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: m.response()}
	close(ch)
	return ch, nil
}

func (m *mockProvider) response() string {
	if m != nil && strings.TrimSpace(m.message) != "" {
		return m.message
	}
	return mockSetupGuide
}

func buildLLMProvider(cfg *config.Config) llm.Provider {
	// All normal provider construction starts from modelruntime so config,
	// provider profiles, auth reuse, and legacy compatibility share one path.
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{})
	if err == nil {
		if provider := buildProviderFromRuntime(rt); provider != nil {
			return provider
		}
	}
	if err != nil {
		log.Warn("no LLM provider resolved, using mock provider", "error", err, "hint", "run `selfmind model` or edit config.yaml")
	} else {
		log.Warn("no LLM provider adapter available, using mock provider", "hint", "run `selfmind model` or edit config.yaml")
	}
	// The setup-diagnostic mock must still pass through MaybeWrapVCR: in eval
	// replay mode (selfcheck, CI) the VCR wrapper is what serves recorded
	// cassettes, and it normally rides the transport construction inside
	// buildProviderFromRuntime — which never ran when no credentials resolved.
	// Without this wrap a credential-less environment silently bypassed every
	// cassette: CI's offline gate answered from the mock instead of the
	// recording and failed 8 replay cases on every push while the same gate
	// passed on any machine with credentials (observed live 2026-07-27). The
	// wrap is a no-op outside VCR/flight modes, so production is untouched.
	return llm.MaybeWrapVCR(&mockProvider{message: modelSetupDiagnostic(cfg, err)})
}

// ResolveModelDisplay returns the runtime model metadata that the TUI should
// show. It intentionally uses the resolver instead of raw config values so the
// UI does not claim a model is active when provider construction fell back to
// the setup diagnostic.
func ResolveModelDisplay(cfg *config.Config) (provider, model string, configured bool) {
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{})
	if err != nil {
		return "not configured", "", false
	}
	if buildProviderFromRuntime(rt) == nil {
		if rt.Provider != "" {
			return rt.Provider, firstNonEmpty(rt.Model, "unsupported"), false
		}
		return "unsupported", firstNonEmpty(rt.Model, ""), false
	}
	return firstNonEmpty(rt.Provider, "default"), firstNonEmpty(rt.Model, "active"), true
}

func modelSetupDiagnostic(cfg *config.Config, resolveErr error) string {
	provider := ""
	model := ""
	path := ""
	if cfg != nil {
		provider = cfg.EffectiveProvider()
		model = cfg.EffectiveModel()
		path = cfg.Path
	}
	if strings.TrimSpace(provider) == "" && strings.TrimSpace(model) == "" && resolveErr == nil {
		return mockSetupGuide
	}

	var sb strings.Builder
	sb.WriteString("SelfMind could not start the configured AI model.\n\n")
	if provider != "" || model != "" {
		sb.WriteString(fmt.Sprintf("Configured: provider=%s model=%s\n", valueOrDash(provider), valueOrDash(model)))
	}
	if path != "" {
		sb.WriteString("Config: " + path + "\n")
	}
	if resolveErr != nil {
		sb.WriteString("Reason: " + resolveErr.Error() + "\n")
	} else {
		sb.WriteString("Reason: provider resolved, but no adapter was available for this protocol.\n")
	}
	sb.WriteString("\nRun:\n  selfmind model\n\n")
	sb.WriteString("Choose or reselect the route in Model Manager; every completed selection is validated automatically.")
	return sb.String()
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func defaultProviderName(cfg *config.Config) string {
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{})
	if err == nil && strings.TrimSpace(rt.Provider) != "" {
		return rt.Provider
	}

	pName := strings.ToLower(strings.TrimSpace(cfg.EffectiveProvider()))
	if pName != "" {
		return pName
	}
	switch {
	case cfg.Providers.Anthropic.APIKey != "":
		return "anthropic"
	case cfg.Providers.Google.APIKey != "":
		return "google"
	case cfg.Providers.OpenAI.APIKey != "":
		return "openai"
	case cfg.Providers.MiniMaxAPIKey != "":
		return "minimax"
	case cfg.Providers.OpenRouterAPIKey != "":
		return "openrouter"
	default:
		return "mock"
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func codingContextLength(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	primary := cfg.EffectivePrimary()
	selection := modelruntime.Selection{
		Provider: primary.Provider, Model: primary.Model, ContextLength: primary.ContextLength,
		ReasoningEffort: primary.Reasoning, ServiceTier: primary.ServiceTier,
	}
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), selection)
	if err != nil {
		return 0
	}
	return rt.ContextLength
}

// summarizerOutputLimit returns the actual resolved route capacity. The kernel
// applies its own bounded ceiling and retry policy; this prevents an explicitly
// smaller role/provider limit from being ignored.
func summarizerOutputLimit(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	roleCfg, _, ok := cfg.ResolveAuxiliaryRole(string(llm.RoleSummarizer))
	if !ok || roleConfigEmpty(roleCfg) {
		return 0
	}
	providerName := firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg))
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), roleProviderSelection(llm.RoleSummarizer, providerName, roleCfg))
	if err == nil && rt.MaxTokens > 0 {
		return rt.MaxTokens
	}
	return roleCfg.MaxTokens
}

func llmQuirks(q modelruntime.ProviderQuirks) llm.ProviderQuirks {
	return llm.ProviderQuirks{
		AuthHeader:        q.AuthHeader,
		ToolSchema:        q.ToolSchema,
		SystemMessageMode: q.SystemMessageMode,
		ThinkingMode:      q.ThinkingMode,
		UserIdentityField: q.UserIdentityField,
		UserAgent:         q.UserAgent,
		HTTPVersion:       q.HTTPVersion,
		DisableHTTP2:      q.DisableHTTP2,
		PromptCache:       q.PromptCache,
		SupportsTools:     q.SupportsTools,
		SupportsStreaming: q.SupportsStreaming,
		SupportsVision:    q.SupportsVision,
	}
}

func runtimeQuirksFromConfig(q config.ProviderQuirks) modelruntime.ProviderQuirks {
	out := modelruntime.ProviderQuirks{
		AuthHeader:        q.AuthHeader,
		ToolSchema:        q.ToolSchema,
		SystemMessageMode: q.SystemMessageMode,
		ThinkingMode:      q.ThinkingMode,
		UserIdentityField: q.UserIdentityField,
		UserAgent:         q.UserAgent,
		HTTPVersion:       q.HTTPVersion,
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

func emptyConfigQuirks(q config.ProviderQuirks) bool {
	return strings.TrimSpace(q.AuthHeader) == "" &&
		strings.TrimSpace(q.ToolSchema) == "" &&
		strings.TrimSpace(q.SystemMessageMode) == "" &&
		strings.TrimSpace(q.ThinkingMode) == "" &&
		strings.TrimSpace(q.UserIdentityField) == "" &&
		strings.TrimSpace(q.UserAgent) == "" &&
		strings.TrimSpace(q.HTTPVersion) == "" &&
		q.ResponsesStoreFalse == nil &&
		q.ResponsesRequireStream == nil &&
		q.PromptCache == nil
}

func buildProviderFromRuntime(rt modelruntime.Runtime) llm.Provider {
	// modelruntime resolves metadata and credentials; llm owns the protocol
	// transport registry. Keep provider construction behind this boundary so new
	// vendors do not grow app/gateway conditionals.
	return llm.BuildTransportProvider(llm.TransportConfig{
		Provider:               rt.Provider,
		Protocol:               rt.Protocol,
		Model:                  rt.Model,
		BaseURL:                rt.BaseURL,
		APIKey:                 rt.APIKey,
		KeyGetter:              rt.TokenGetter,
		TokenRefresher:         rt.TokenRefresher,
		Headers:                rt.Headers,
		ExtraBody:              rt.ExtraBody,
		ExtraQuery:             rt.ExtraQuery,
		MaxTokens:              rt.MaxTokens,
		ReasoningEffort:        rt.ReasoningEffort,
		Thinking:               rt.Thinking,
		ServiceTier:            rt.ServiceTier,
		Quirks:                 llmQuirks(rt.Quirks),
		ResponsesStoreFalse:    rt.Quirks.ResponsesStoreFalse,
		ResponsesRequireStream: rt.Quirks.ResponsesRequireStream,
	})
}

func buildProviderForSelection(cfg *config.Config, providerName, model, baseURL, apiKey string) llm.Provider {
	return buildProviderForSelectionWithRuntime(cfg, modelruntime.Selection{
		Provider: providerName,
		Model:    model,
		BaseURL:  baseURL,
		APIKey:   apiKey,
	})
}

func buildProviderForSelectionWithRuntime(cfg *config.Config, selection modelruntime.Selection) llm.Provider {
	// Model roles and CLI overrides use the same resolver as the default agent;
	// the legacy switch below exists only for older config shapes.
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{
		Provider:        selection.Provider,
		Model:           selection.Model,
		BaseURL:         selection.BaseURL,
		Protocol:        selection.Protocol,
		APIKey:          selection.APIKey,
		Headers:         selection.Headers,
		ExtraHeaders:    selection.ExtraHeaders,
		ExtraBody:       selection.ExtraBody,
		ExtraQuery:      selection.ExtraQuery,
		ContextLength:   selection.ContextLength,
		MaxTokens:       selection.MaxTokens,
		ReasoningEffort: selection.ReasoningEffort,
		Thinking:        selection.Thinking,
		ServiceTier:     selection.ServiceTier,
		Quirks:          selection.Quirks,
	})
	if err == nil {
		return buildProviderFromRuntime(rt)
	}

	providerName := selection.Provider
	model := selection.Model
	baseURL := selection.BaseURL
	apiKey := selection.APIKey
	pType := strings.ToLower(strings.TrimSpace(providerName))
	switch pType {
	case "anthropic":
		key := firstNonEmpty(apiKey, cfg.Providers.Anthropic.APIKey)
		if key == "" {
			return nil
		}
		ad := llm.NewAnthropicAdapter(key)
		if model != "" {
			ad.Model = model
		}
		ad.BaseURL = anthropicMessagesURL(firstNonEmpty(baseURL, cfg.Providers.Anthropic.BaseURL))
		return ad
	case "openai":
		key := firstNonEmpty(apiKey, cfg.Providers.OpenAI.APIKey)
		if key == "" {
			return nil
		}
		ad := llm.NewOpenAIAdapter(key)
		if model != "" {
			ad.Model = model
		}
		ad.BaseURL = chatCompletionsURL(firstNonEmpty(baseURL, cfg.Providers.OpenAI.BaseURL))
		return ad
	case "openrouter":
		key := firstNonEmpty(apiKey, cfg.Providers.OpenRouterAPIKey)
		if key == "" {
			return nil
		}
		ad := llm.NewOpenRouterAdapter(key)
		if model != "" {
			ad.Model = model
		}
		if baseURL != "" {
			ad.BaseURL = baseURL
		}
		return ad
	case "gemini", "google":
		key := firstNonEmpty(apiKey, cfg.Providers.Google.APIKey)
		if key == "" {
			return nil
		}
		ad := llm.NewGeminiAdapter(key)
		if model != "" {
			ad.Model = model
		}
		ad.BaseURL = googleChatCompletionsURL(firstNonEmpty(baseURL, cfg.Providers.Google.BaseURL))
		return ad
	case "minimax":
		key := firstNonEmpty(apiKey, cfg.Providers.MiniMaxAPIKey)
		if key == "" {
			return nil
		}
		ad := llm.NewMiniMaxAdapter(key)
		if model != "" {
			ad.Model = model
		}
		if baseURL != "" {
			ad.BaseURL = baseURL
		}
		return ad
	}

	for _, cp := range cfg.Providers.Custom {
		if customProviderMatches(cp.Name, providerName) {
			key := firstNonEmpty(apiKey, cp.APIKey)
			selectedURL := firstNonEmpty(baseURL, cp.BaseURL)
			selectedModel := firstNonEmpty(model, cp.Model)
			return llm.NewGenericOpenAIAdapter(cp.Name, chatCompletionsURL(selectedURL), key, selectedModel)
		}
	}
	return nil
}

func customProviderMatches(name, requested string) bool {
	name = strings.TrimSpace(name)
	requested = strings.TrimSpace(requested)
	if strings.EqualFold(name, requested) {
		return true
	}
	return strings.EqualFold("custom:"+name, requested)
}

func chatCompletionsURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "https://api.openai.com/v1/chat/completions"
	}
	if strings.HasSuffix(strings.ToLower(baseURL), "/chat/completions") {
		return baseURL
	}
	return baseURL + "/chat/completions"
}

func anthropicMessagesURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "https://api.anthropic.com/v1/messages"
	}
	lower := strings.ToLower(baseURL)
	if strings.HasSuffix(lower, "/v1/messages") || strings.HasSuffix(lower, "/messages") {
		return baseURL
	}
	if strings.HasSuffix(lower, "/v1") {
		return baseURL + "/messages"
	}
	return baseURL + "/v1/messages"
}

func googleChatCompletionsURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	}
	lower := strings.ToLower(baseURL)
	if strings.HasSuffix(lower, "/chat/completions") {
		return baseURL
	}
	if strings.Contains(lower, "generativelanguage.googleapis.com") && !strings.Contains(lower, "/openai") {
		return baseURL + "/openai/chat/completions"
	}
	return chatCompletionsURL(baseURL)
}

func buildKeyGetter(mem *memory.MemoryManager, tenantID, provider string) func() string {
	return func() string {
		if mem == nil {
			return ""
		}
		// Return a value only when the database actually has one; otherwise return
		// empty so the adapter falls back to its default.
		val, err := mem.GetSecret(context.Background(), tenantID, provider+"_api_key")
		if err != nil || val == "" {
			return ""
		}
		return val
	}
}

func applyDynamicKeyGetter(provider llm.Provider, mem *memory.MemoryManager, tenantID, providerName string) {
	// SaaS/user-level secrets can override process config at call time without
	// rebuilding adapters or leaking tenant-specific keys into global state.
	if provider == nil || providerName == "" {
		return
	}
	getter := buildKeyGetter(mem, tenantID, strings.ToLower(providerName))
	switch p := provider.(type) {
	case *llm.AnthropicAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	case *llm.OpenAIAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	case *llm.GeminiAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	case *llm.MiniMaxAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	case *llm.GenericOpenAIAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	case *llm.OpenRouterAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	case *llm.ResponsesAdapter:
		p.KeyGetter = chainKeyGetter(p.KeyGetter, getter)
	}
}

func chainKeyGetter(existing, override func() string) func() string {
	if existing == nil {
		return override
	}
	return func() string {
		if override != nil {
			if value := override(); value != "" {
				return value
			}
		}
		return existing()
	}
}

func buildModelGateway(cfg *config.Config, mem *memory.MemoryManager, tenantID string, fallbackProvider llm.Provider) *llm.PolicyGateway {
	// Role profiles let expensive/slow jobs such as review or memory extraction
	// use different models while the main coding agent keeps its default model.
	pName := defaultProviderName(cfg)
	applyDynamicKeyGetter(fallbackProvider, mem, tenantID, pName)

	fallbackProfile := llm.ProviderProfile{
		Name:         "default",
		ProviderName: pName,
		Model:        firstNonEmpty(cfg.EffectiveModel(), llm.GetModelName(fallbackProvider)),
		Provider:     fallbackProvider,
	}
	gateway := llm.NewPolicyGateway(fallbackProfile)

	registered := make(map[string]struct{})
	for _, role := range auxiliaryModelRoles() {
		roleCfg, _, ok := cfg.ResolveAuxiliaryRole(string(role))
		if !ok || roleConfigEmpty(roleCfg) {
			continue
		}
		registerModelRoleProfile(gateway, cfg, mem, tenantID, pName, role, roleCfg)
		registered[string(role)] = struct{}{}
	}

	for roleName, roleCfg := range cfg.Models.Roles {
		roleName = strings.TrimSpace(roleName)
		if roleName == "" {
			continue
		}
		if roleConfigEmpty(roleCfg) {
			continue
		}
		if roleName == string(llm.RoleCodingAgent) {
			// models.primary is the only foreground authority. Older configs may
			// still contain this former override; keep it inert instead of silently
			// replacing the Main model shown by the manager.
			continue
		}
		if _, ok := registered[roleName]; ok {
			continue
		}
		registerModelRoleProfile(gateway, cfg, mem, tenantID, pName, llm.ModelRole(roleName), roleCfg)
	}
	return gateway
}

func registerModelRoleProfile(gateway *llm.PolicyGateway, cfg *config.Config, mem *memory.MemoryManager,
	tenantID, primaryProvider string, role llm.ModelRole, roleCfg config.ModelRoleConfig) {
	roleProviderName := firstNonEmpty(roleCfg.Provider, primaryProvider)
	roleProvider := buildRoleProvider(cfg, role, roleProviderName, roleCfg)
	if roleProvider == nil {
		log.Warn("model role skipped: provider unavailable", "role", role, "provider", roleProviderName)
		return
	}
	applyDynamicKeyGetter(roleProvider, mem, tenantID, roleProviderName)
	gateway.RegisterRoleProfile(role, llm.ProviderProfile{
		Name:         string(role),
		ProviderName: roleProviderName,
		Model:        firstNonEmpty(roleCfg.Model, llm.GetModelName(roleProvider)),
		Provider:     roleProvider,
	})
}

// roleConfigEmpty reports whether a models.roles entry carries no actual
// override — such entries never register a role profile.
func roleConfigEmpty(roleCfg config.ModelRoleConfig) bool {
	return roleCfg.Provider == "" && roleCfg.Model == "" && roleCfg.BaseURL == "" && roleCfg.Protocol == "" && roleCfg.APIKey == "" &&
		roleCfg.ContextLength <= 0 && roleCfg.MaxTokens <= 0 && len(roleCfg.Headers) == 0 && len(roleCfg.ExtraHeaders) == 0 &&
		len(roleCfg.ExtraBody) == 0 && len(roleCfg.ExtraQuery) == 0 &&
		roleCfg.EffectiveReasoning() == "" && len(roleCfg.Thinking) == 0 && roleCfg.ServiceTier == "" &&
		emptyConfigQuirks(roleCfg.Quirks)
}

// buildRoleProvider resolves one role override through the same
// modelruntime.Resolver path as the default provider (headers, max tokens,
// reasoning effort, thinking, service tier, quirks all carried).
func buildRoleProvider(cfg *config.Config, role llm.ModelRole, roleProviderName string, roleCfg config.ModelRoleConfig) llm.Provider {
	return buildProviderForSelectionWithRuntime(cfg, roleProviderSelection(role, roleProviderName, roleCfg))
}

// roleProviderSelection keeps every role on the provider-defined transport.
// Kimi Coding Plan's /coding route speaks Anthropic Messages for both the main
// agent and bounded auxiliary calls. An explicit role protocol still wins for
// custom gateways or installations with a different wire contract.
func roleProviderSelection(_ llm.ModelRole, roleProviderName string, roleCfg config.ModelRoleConfig) modelruntime.Selection {
	return modelruntime.Selection{
		// Keep an omitted provider omitted so the resolver can inherit the full
		// primary selection. roleProviderName is still used by the caller for
		// registration and credential bookkeeping.
		Provider:        roleCfg.Provider,
		Model:           roleCfg.Model,
		BaseURL:         roleCfg.BaseURL,
		Protocol:        roleCfg.Protocol,
		APIKey:          roleCfg.APIKey,
		Headers:         roleCfg.Headers,
		ExtraHeaders:    roleCfg.ExtraHeaders,
		ExtraBody:       roleCfg.ExtraBody,
		ExtraQuery:      roleCfg.ExtraQuery,
		ContextLength:   roleCfg.ContextLength,
		MaxTokens:       roleCfg.MaxTokens,
		ReasoningEffort: roleCfg.EffectiveReasoning(),
		Thinking:        roleCfg.Thinking,
		ServiceTier:     roleCfg.ServiceTier,
		Quirks:          runtimeQuirksFromConfig(roleCfg.Quirks),
	}
}

// SemanticRecallExpander builds the query expander for the gateway's AUTOMATIC
// recall slice (Work Timeline P2, docs/work-timeline.md "Semantic recall").
// Unlike the agent-side expander behind the model-invoked session_search tool
// (which falls back to the default provider), automatic recall runs at the
// start of every eligible turn — so it only expands when a semantic_recall
// role model is configured explicitly or through models.auxiliary, and never
// spends main-model tokens implicitly.
// Returns nil when the role is unconfigured, its provider cannot be built, or
// memory.semantic_recall is disabled; the recall engine then degrades to
// raw-term FTS.
func SemanticRecallExpander(mem *memory.MemoryManager, cfg *config.Config, tenantID string, prompts *promptassets.Snapshot) *memory.SemanticExpander {
	if cfg == nil || !cfg.Memory.SemanticRecall {
		return nil
	}
	roleCfg, _, ok := cfg.ResolveAuxiliaryRole(string(llm.RoleSemanticRecall))
	if !ok || roleConfigEmpty(roleCfg) {
		return nil
	}
	roleProviderName := firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg))
	provider := buildRoleProvider(cfg, llm.RoleSemanticRecall, roleProviderName, roleCfg)
	if provider == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = "default"
	}
	applyDynamicKeyGetter(provider, mem, tenantID, roleProviderName)
	return memory.NewSemanticExpander(provider, true, prompts)
}

// InitAgent wires one already-validated immutable prompt snapshot. Daemon
// startup passes the same snapshot through agent, tool/delegation, and
// background-role construction; each prompt profile selects only its owned
// sections from that process-frozen snapshot.
func InitAgent(mem *memory.MemoryManager, cfg *config.Config, tenantID string, prompts *promptassets.Snapshot, controlStore *control.Store) (*kernel.Agent, error) {
	provider := buildLLMProvider(cfg)
	if provider == nil {
		return nil, fmt.Errorf("no LLM provider available")
	}

	// Debug logging suppressed to keep the TUI clean.
	/*
		geminiKey := cfg.Providers.GeminiAPIKey
		if len(geminiKey) > 8 {
			fmt.Printf("[Config] Found Gemini Key in YAML: %s...%s\n", geminiKey[:4], geminiKey[len(geminiKey)-4:])
		}
	*/

	// Inject the dynamic key loader into the adapter (per-request token retrieval).
	if tenantID == "" {
		tenantID = "default"
	}
	pName := defaultProviderName(cfg)
	applyDynamicKeyGetter(provider, mem, tenantID, pName)

	modelGateway := buildModelGateway(cfg, mem, tenantID, provider)
	codingProvider := modelGateway.ProviderForRole(llm.RoleCodingAgent)
	reviewProvider := configuredAuxiliaryRoleProvider(mem, cfg, tenantID, llm.RoleBackgroundReview)
	var reviewRoutes []maintenanceRouteIdentity
	semanticRecallProvider := configuredAuxiliaryRoleProvider(mem, cfg, tenantID, llm.RoleSemanticRecall)
	summaryProvider := configuredAuxiliaryRoleProvider(mem, cfg, tenantID, llm.RoleSummarizer)
	if summaryProvider == nil {
		// Legacy configurations used memory_extract for compaction before the
		// dedicated summarizer role existed.
		summaryProvider = explicitRoleProvider(mem, cfg, tenantID, llm.RoleMemoryExtract)
	}
	// Smart-mode approval triage is latency-sensitive foreground policy work.
	// Prefer fast_classifier resolved through auxiliary/role config and retain
	// background_review only as a legacy config fallback; never borrow the main
	// coding model silently.
	judgeProvider, _ := configuredApprovalJudgeProvider(mem, cfg, tenantID)
	if controlStore != nil {
		// Background learning shares the same durable physical-route circuit as
		// post-run analysis and memory consolidation, and the same two-position
		// chain: the role's own route, then the models.auxiliary floor. In daemon
		// mode it must not silently borrow the foreground coding model.
		reviewProvider, reviewRoutes = configuredMaintenanceProvider(mem, cfg, tenantID, controlStore, llm.RoleBackgroundReview)
		if replayed, err := controlStore.RequeueBlockedJobsForHealthyProviderRoutesAcrossTenants(context.Background(), tenantID, 100, maintenanceRouteIDs(reviewRoutes), time.Now()); err != nil {
			log.Warn("background review: failed to migrate jobs to a healthy fallback route", "error", err)
		} else if replayed > 0 {
			log.Info("background review: replaying jobs on a healthy fallback route", "jobs", replayed)
		}
	}

	skillStorage, skillStorageErr := configuredSkillStorage(cfg)
	if skillStorageErr != nil {
		return nil, skillStorageErr
	}
	skillsDir := tools.SkillsDirForTenant(skillStorage.BaseDir(), tenantID)

	maxIter := cfg.Agent.MaxIterations
	if maxIter == 0 {
		maxIter = 90
	}
	maxRetries := cfg.Agent.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}

	// Legacy ReflectionEngine Skill writes are deliberately absent from the
	// daemon path. The durable cohort-driven skill curator is the sole proposal
	// authority; keeping a second writer here would bypass candidate evidence.
	agent := kernel.NewAgent(mem, nil, codingProvider, cfg.Agent.Soul, maxIter, maxRetries, nil)
	agent.SetPromptSnapshot(prompts)
	agent.SetToolBudgetPolicy(kernel.ToolBudgetPolicy{
		Initial:       cfg.Agent.ActionToolBudget,
		Step:          cfg.Agent.ActionToolBudgetStep,
		Limit:         cfg.Agent.ActionToolBudgetLimit,
		MaxExtensions: cfg.Agent.MaxBudgetExtensions,
	})
	// LLM transport resilience (Package Zero): backoff/attempt policy for the
	// agent retry loop, plus the process-wide SSE idle watchdog default.
	applyLLMResilience(cfg)
	llmRetries := cfg.Agent.LLMMaxRetries
	if llmRetries <= 0 {
		llmRetries = maxRetries
	}
	if llmRetries <= 0 {
		llmRetries = 5
	}
	agent.SetRetryPolicy(
		llmRetries,
		parseResilienceDuration(cfg.Agent.LLMRetryBase, llm.DefaultRetryBase),
		parseResilienceDuration(cfg.Agent.LLMRetryCap, llm.DefaultRetryCap),
	)
	agent.SetContextWindow(codingContextLength(cfg))
	// Over-budget context compaction uses the auxiliary/dedicated summarizer
	// (kept OFF the main coding provider) instead of dropping oldest turns.
	agent.SetSummaryProvider(summaryProvider)
	agent.SetSummaryOutputLimit(summarizerOutputLimit(cfg))
	// Carry the cheap triage provider so the gateway can build the smart-mode
	// approval judge (H2) from it, without kernel depending on concrete tools.
	agent.SetApprovalJudgeProvider(judgeProvider)
	// Skill candidates and the one active body are selected per work unit by
	// the gateway. Do not inject the tenant-wide catalog into every prompt.
	// Post-run memory extraction and label maintenance are intentionally owned
	// by the daemon's single PostRunAnalyzer. The Agent must not launch a second
	// extractor/profile model call for the same run.
	reviewEngine := kernel.NewBackgroundReviewEngine(mem, nil, reviewProvider, kernel.EvolutionConfig{
		Enabled:                cfg.Evolution.Enabled,
		Mode:                   cfg.Evolution.Mode,
		MinComplexityThreshold: cfg.Evolution.MinComplexityThreshold,
		AutoArchiveConfidence:  cfg.Evolution.AutoArchiveConfidence,
		NudgeInterval:          cfg.Evolution.NudgeInterval,
		SkillsDir:              skillsDir,
	}, 8, maxRetries)
	reviewEngine.SetControlTenantID(tenantID)
	reviewEngine.SetPromptSnapshot(prompts)
	agent.SetBackgroundReviewEngine(reviewEngine)

	// Configure the nudge interval.
	if cfg.Evolution.NudgeInterval > 0 {
		agent.SetNudgeInterval(cfg.Evolution.NudgeInterval)
	}

	// Inject the semantic query expander.
	se := memory.NewSemanticExpander(semanticRecallProvider, cfg.Memory.SemanticRecall, prompts)
	agent.SetSemanticExpander(se)

	// Configure the memory injection format.
	agent.SetUseMemoryFence(cfg.Memory.UseMemoryFence)

	return agent, nil
}
