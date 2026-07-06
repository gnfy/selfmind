package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
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

/*
const mockSetupGuide = `SelfMind 尚未配置 API Key，无法进行 AI 对话。

请按以下步骤配置：

1. 编辑配置文件：
   nano ~/.selfmind/config.yaml

2. 在 providers 区块添加你的 API Key，例如：
   providers:
     anthropic_api_key: "sk-ant-your-key-here"
   （或使用 openai_api_key / gemini_api_key / minimax_api_key）

3. 重启 SelfMind

获取 API Key：
  - Anthropic: https://console.anthropic.com/
  - OpenAI: https://platform.openai.com/
  - Gemini: https://aistudio.google.com/
  - MiniMax: https://platform.minimaxi.com/

配置完成后，SelfMind 将自动使用配置的模型。`

`

*/

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
	return &mockProvider{message: modelSetupDiagnostic(cfg, err)}
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
	sb.WriteString("\nRun:\n  selfmind model check\n\n")
	sb.WriteString("If the check succeeds, make sure the TUI is launched with the same binary and config path.")
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

// skillInventoryFor returns a closure that renders the tenant's learned skills
// as a compact prompt block (name + short description, capped). It returns ""
// when there are no skills, so nothing is injected for a fresh tenant.
func skillInventoryFor(defaultTenantID string) func(string) string {
	return func(tid string) string {
		if strings.TrimSpace(tid) == "" {
			tid = defaultTenantID
		}
		skills, err := tools.ListSkillsForTenant(tid, false)
		if err != nil || len(skills) == 0 {
			return ""
		}
		const maxSkills = 20
		var sb strings.Builder
		sb.WriteString("# LEARNED SKILLS\n")
		sb.WriteString("You have these reusable skills from past work. When one fits the task, load it with skill_view before reinventing the approach.\n")
		shown := 0
		for _, s := range skills {
			name := strings.TrimSpace(s.Name)
			if name == "" {
				continue
			}
			desc := strings.TrimSpace(s.Description)
			if r := []rune(desc); len(r) > 60 {
				desc = string(r[:60]) + "…"
			}
			if desc != "" {
				fmt.Fprintf(&sb, "- %s: %s\n", name, desc)
			} else {
				fmt.Fprintf(&sb, "- %s\n", name)
			}
			shown++
			if shown >= maxSkills {
				if remaining := len(skills) - shown; remaining > 0 {
					fmt.Fprintf(&sb, "- … (%d more; use skills_list to see all)\n", remaining)
				}
				break
			}
		}
		if shown == 0 {
			return ""
		}
		return sb.String()
	}
}

func codingContextLength(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	selection := modelruntime.Selection{}
	if roleCfg, ok := cfg.Models.Roles[string(llm.RoleCodingAgent)]; ok {
		selection = modelruntime.Selection{
			Provider:        roleCfg.Provider,
			Model:           roleCfg.Model,
			BaseURL:         roleCfg.BaseURL,
			APIKey:          roleCfg.APIKey,
			Headers:         roleCfg.Headers,
			ContextLength:   roleCfg.ContextLength,
			MaxTokens:       roleCfg.MaxTokens,
			ReasoningEffort: roleCfg.ReasoningEffort,
			Thinking:        roleCfg.Thinking,
			ServiceTier:     roleCfg.ServiceTier,
			Quirks:          runtimeQuirksFromConfig(roleCfg.Quirks),
		}
	}
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), selection)
	if err != nil {
		return 0
	}
	return rt.ContextLength
}

func llmQuirks(q modelruntime.ProviderQuirks) llm.ProviderQuirks {
	return llm.ProviderQuirks{
		AuthHeader:        q.AuthHeader,
		ToolSchema:        q.ToolSchema,
		SystemMessageMode: q.SystemMessageMode,
		ThinkingMode:      q.ThinkingMode,
		UserAgent:         q.UserAgent,
		DisableHTTP2:      q.DisableHTTP2,
		SupportsTools:     q.SupportsTools,
		SupportsStreaming: q.SupportsStreaming,
		SupportsVision:    q.SupportsVision,
	}
}

func runtimeQuirksFromConfig(q config.ProviderQuirks) modelruntime.ProviderQuirks {
	return modelruntime.ProviderQuirks{
		AuthHeader:             q.AuthHeader,
		ToolSchema:             q.ToolSchema,
		SystemMessageMode:      q.SystemMessageMode,
		ThinkingMode:           q.ThinkingMode,
		UserAgent:              q.UserAgent,
		ResponsesStoreFalse:    q.ResponsesStoreFalse,
		ResponsesRequireStream: q.ResponsesRequireStream,
	}
}

func emptyConfigQuirks(q config.ProviderQuirks) bool {
	return strings.TrimSpace(q.AuthHeader) == "" &&
		strings.TrimSpace(q.ToolSchema) == "" &&
		strings.TrimSpace(q.SystemMessageMode) == "" &&
		strings.TrimSpace(q.ThinkingMode) == "" &&
		strings.TrimSpace(q.UserAgent) == "" &&
		!q.ResponsesStoreFalse &&
		!q.ResponsesRequireStream
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
		APIKey:          selection.APIKey,
		Headers:         selection.Headers,
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
		// 只有当数据库里确实有值时才返回，否则返回空让 Adapter 使用默认值
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

	for roleName, roleCfg := range cfg.Models.Roles {
		roleName = strings.TrimSpace(roleName)
		if roleName == "" {
			continue
		}
		if roleConfigEmpty(roleCfg) {
			continue
		}

		roleProviderName := firstNonEmpty(roleCfg.Provider, pName)
		roleProvider := buildRoleProvider(cfg, roleProviderName, roleCfg)
		if roleProvider == nil {
			log.Warn("model role skipped: provider unavailable", "role", roleName, "provider", roleProviderName)
			continue
		}
		applyDynamicKeyGetter(roleProvider, mem, tenantID, roleProviderName)

		gateway.RegisterRoleProfile(llm.ModelRole(roleName), llm.ProviderProfile{
			Name:         roleName,
			ProviderName: roleProviderName,
			Model:        firstNonEmpty(roleCfg.Model, llm.GetModelName(roleProvider)),
			Provider:     roleProvider,
		})
	}
	return gateway
}

// roleConfigEmpty reports whether a models.roles entry carries no actual
// override — such entries never register a role profile.
func roleConfigEmpty(roleCfg config.ModelRoleConfig) bool {
	return roleCfg.Provider == "" && roleCfg.Model == "" && roleCfg.BaseURL == "" && roleCfg.APIKey == "" &&
		roleCfg.ContextLength <= 0 && roleCfg.MaxTokens <= 0 && len(roleCfg.Headers) == 0 &&
		roleCfg.ReasoningEffort == "" && len(roleCfg.Thinking) == 0 && roleCfg.ServiceTier == "" &&
		emptyConfigQuirks(roleCfg.Quirks)
}

// buildRoleProvider resolves one role override through the same
// modelruntime.Resolver path as the default provider (headers, max tokens,
// reasoning effort, thinking, service tier, quirks all carried).
func buildRoleProvider(cfg *config.Config, roleProviderName string, roleCfg config.ModelRoleConfig) llm.Provider {
	return buildProviderForSelectionWithRuntime(cfg, modelruntime.Selection{
		Provider:        roleProviderName,
		Model:           roleCfg.Model,
		BaseURL:         roleCfg.BaseURL,
		APIKey:          roleCfg.APIKey,
		Headers:         roleCfg.Headers,
		ContextLength:   roleCfg.ContextLength,
		MaxTokens:       roleCfg.MaxTokens,
		ReasoningEffort: roleCfg.ReasoningEffort,
		Thinking:        roleCfg.Thinking,
		ServiceTier:     roleCfg.ServiceTier,
		Quirks:          runtimeQuirksFromConfig(roleCfg.Quirks),
	})
}

// SemanticRecallExpander builds the query expander for the gateway's AUTOMATIC
// recall slice (Work Timeline P2, docs/work-timeline.md "Semantic recall").
// Unlike the agent-side expander behind the model-invoked session_search tool
// (which falls back to the default provider), automatic recall runs at the
// start of every eligible turn — so it only expands when a semantic_recall
// role model is EXPLICITLY configured, and never spends main-model tokens.
// Returns nil when the role is unconfigured, its provider cannot be built, or
// memory.semantic_recall is disabled; the recall engine then degrades to
// raw-term FTS.
func SemanticRecallExpander(mem *memory.MemoryManager, cfg *config.Config, tenantID string) *memory.SemanticExpander {
	if cfg == nil || !cfg.Memory.SemanticRecall {
		return nil
	}
	roleCfg, ok := cfg.Models.Roles[string(llm.RoleSemanticRecall)]
	if !ok || roleConfigEmpty(roleCfg) {
		return nil
	}
	roleProviderName := firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg))
	provider := buildRoleProvider(cfg, roleProviderName, roleCfg)
	if provider == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = "default"
	}
	applyDynamicKeyGetter(provider, mem, tenantID, roleProviderName)
	return memory.NewSemanticExpander(provider, true)
}

// InitAgent creates the LLM provider, reflection engine, and agent core.
func InitAgent(mem *memory.MemoryManager, cfg *config.Config, tenantID string) (*kernel.Agent, error) {
	provider := buildLLMProvider(cfg)
	if provider == nil {
		return nil, fmt.Errorf("no LLM provider available")
	}

	// 安全打印调试信息 (Logs suppressed for clean TUI)
	/*
		geminiKey := cfg.Providers.GeminiAPIKey
		if len(geminiKey) > 8 {
			fmt.Printf("[Config] Found Gemini Key in YAML: %s...%s\n", geminiKey[:4], geminiKey[len(geminiKey)-4:])
		}
	*/

	// 关键修复：将动态 Key 加载器注入适配器
	if tenantID == "" {
		tenantID = "default"
	}
	pName := defaultProviderName(cfg)
	applyDynamicKeyGetter(provider, mem, tenantID, pName)

	modelGateway := buildModelGateway(cfg, mem, tenantID, provider)
	codingProvider := modelGateway.ProviderForRole(llm.RoleCodingAgent)
	reviewProvider := modelGateway.ProviderForRole(llm.RoleBackgroundReview)
	memoryExtractProvider := modelGateway.ProviderForRole(llm.RoleMemoryExtract)
	skillCuratorProvider := modelGateway.ProviderForRole(llm.RoleSkillCurator)
	semanticRecallProvider := modelGateway.ProviderForRole(llm.RoleSemanticRecall)
	fastProvider := modelGateway.ProviderForRole(llm.RoleFastClassifier)
	// Smart-mode approval triage (H2) uses the background_review role: a cheap
	// side-task model kept OFF the main coding provider. Falls back to the
	// default model when no background_review role is configured.
	judgeProvider := modelGateway.ProviderForRole(llm.RoleBackgroundReview)

	skillsBaseDir := cfg.Evolution.SkillsDir
	if skillsBaseDir == "" {
		home, _ := os.UserHomeDir()
		skillsBaseDir = filepath.Join(home, ".selfmind")
	}
	skillsDir := tools.SkillsDirForTenant(skillsBaseDir, tenantID)

	refl := kernel.NewReflectionEngine(skillCuratorProvider, kernel.EvolutionConfig{
		Enabled:                cfg.Evolution.Enabled,
		Mode:                   cfg.Evolution.Mode,
		MinComplexityThreshold: cfg.Evolution.MinComplexityThreshold,
		AutoArchiveConfidence:  cfg.Evolution.AutoArchiveConfidence,
		NudgeInterval:          cfg.Evolution.NudgeInterval,
		SkillsDir:              skillsDir,
	})

	// 设置 evolution notify channel（暂时传 nil，后续由 TUI 层注入）
	refl.SetNotifyChannel(nil)

	maxIter := cfg.Agent.MaxIterations
	if maxIter == 0 {
		maxIter = 90
	}
	maxRetries := cfg.Agent.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}

	agent := kernel.NewAgent(mem, nil, codingProvider, cfg.Agent.Soul, maxIter, maxRetries, refl)
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
	// Simple direct-answer turns use the fast-classifier role (falls back to the
	// default model when no fast model is configured).
	agent.SetFastProvider(fastProvider)
	// Over-budget context compaction runs by default on the cheap memory_extract
	// role (kept OFF the main coding provider) instead of dropping oldest turns.
	agent.SetSummaryProvider(memoryExtractProvider)
	// Carry the cheap triage provider so the gateway can build the smart-mode
	// approval judge (H2) from it, without kernel depending on concrete tools.
	agent.SetApprovalJudgeProvider(judgeProvider)
	// Surface learned skills in the prompt so the agent reuses what it learned.
	agent.SetSkillInventory(skillInventoryFor(tenantID))
	// Distill accumulated facts into a coherent user profile (uses the
	// memory_extract role; falls back to the default model when unconfigured).
	agent.SetProfileSynthesizer(kernel.NewProfileSynthesizer(memoryExtractProvider, true))
	reviewEngine := kernel.NewBackgroundReviewEngine(mem, nil, reviewProvider, kernel.EvolutionConfig{
		Enabled:                cfg.Evolution.Enabled,
		Mode:                   cfg.Evolution.Mode,
		MinComplexityThreshold: cfg.Evolution.MinComplexityThreshold,
		AutoArchiveConfidence:  cfg.Evolution.AutoArchiveConfidence,
		NudgeInterval:          cfg.Evolution.NudgeInterval,
		SkillsDir:              skillsDir,
	}, 8, maxRetries)
	agent.SetBackgroundReviewEngine(reviewEngine)

	// 设置 nudge interval
	if cfg.Evolution.NudgeInterval > 0 {
		agent.SetNudgeInterval(cfg.Evolution.NudgeInterval)
	}

	// 注入自动事实提取器（默认开启，使用当前 provider）
	fe := kernel.NewFactExtractor(memoryExtractProvider, true)
	agent.SetFactExtractor(fe)

	// 注入每轮轻量提取器（频率控制，使用当前 provider）
	te := kernel.NewTurnExtractor(memoryExtractProvider, true, cfg.Memory.AutoExtractInterval, cfg.Memory.AutoExtractMinChars)
	agent.SetTurnExtractor(te)

	// 注入语义查询扩展器
	se := memory.NewSemanticExpander(semanticRecallProvider, cfg.Memory.SemanticRecall)
	agent.SetSemanticExpander(se)

	// 设置记忆注入格式
	agent.SetUseMemoryFence(cfg.Memory.UseMemoryFence)

	return agent, nil
}
