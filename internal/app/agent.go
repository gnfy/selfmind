package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// mockProvider is used when no LLM API key is configured.
type mockProvider struct{}

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
	return mockSetupGuide, nil
}

func (m *mockProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: mockSetupGuide}, nil
}

func (m *mockProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: mockSetupGuide}
	close(ch)
	return ch, nil
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
	return &mockProvider{}
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

func buildProviderFromRuntime(rt modelruntime.Runtime) llm.Provider {
	// Keep this switch as the narrow app-layer boundary: modelruntime resolves
	// metadata and credentials, while llm adapters only speak provider protocols.
	model := strings.TrimSpace(rt.Model)
	switch modelruntime.NormalizeProtocol(rt.Protocol) {
	case modelruntime.ProtocolAnthropic:
		ad := llm.NewAnthropicAdapter(rt.APIKey)
		if model != "" {
			ad.Model = model
		}
		ad.BaseURL = anthropicMessagesURL(rt.BaseURL)
		return ad
	case modelruntime.ProtocolResponses:
		return llm.NewResponsesAdapter(rt.APIKey, rt.BaseURL, model)
	case modelruntime.ProtocolOpenAIChat:
		ad := llm.NewOpenAIAdapter(rt.APIKey)
		if model != "" {
			ad.Model = model
		}
		ad.BaseURL = chatCompletionsURL(rt.BaseURL)
		return ad
	case modelruntime.ProtocolOpenAICompatible:
		provider := strings.ToLower(strings.TrimSpace(rt.Provider))
		if provider == "openrouter" {
			ad := llm.NewOpenRouterAdapter(rt.APIKey)
			if model != "" {
				ad.Model = model
			}
			ad.BaseURL = chatCompletionsURL(rt.BaseURL)
			return ad
		}
		if provider == "google" || provider == "gemini" || provider == "gemini-cli" {
			ad := llm.NewGeminiAdapter(rt.APIKey)
			if model != "" {
				ad.Model = model
			}
			ad.BaseURL = googleChatCompletionsURL(rt.BaseURL)
			return ad
		}
		return llm.NewGenericOpenAIAdapter(rt.Provider, chatCompletionsURL(rt.BaseURL), rt.APIKey, model)
	default:
		return nil
	}
}

func buildProviderForSelection(cfg *config.Config, providerName, model, baseURL, apiKey string) llm.Provider {
	// Model roles and CLI overrides use the same resolver as the default agent;
	// the legacy switch below exists only for older config shapes.
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{
		Provider: providerName,
		Model:    model,
		BaseURL:  baseURL,
		APIKey:   apiKey,
	})
	if err == nil {
		return buildProviderFromRuntime(rt)
	}

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

func getEffectiveAPIKey(mem *memory.MemoryManager, tenantID, provider string, systemKey string) string {
	if mem == nil {
		return systemKey
	}
	// 优先从数据库加载该租户的 Key
	userKey, err := mem.GetPermission(context.Background(), tenantID, provider+"_api_key")
	// 这里目前复用了 GetPermission 的 bool 返回作为演示
	_ = userKey
	_ = err
	return systemKey
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
		p.KeyGetter = getter
	case *llm.OpenAIAdapter:
		p.KeyGetter = getter
	case *llm.GeminiAdapter:
		p.KeyGetter = getter
	case *llm.MiniMaxAdapter:
		p.KeyGetter = getter
	case *llm.GenericOpenAIAdapter:
		p.KeyGetter = getter
	case *llm.OpenRouterAdapter:
		p.KeyGetter = getter
	case *llm.ResponsesAdapter:
		p.KeyGetter = getter
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
		if roleCfg.Provider == "" && roleCfg.Model == "" && roleCfg.BaseURL == "" && roleCfg.APIKey == "" {
			continue
		}

		roleProviderName := firstNonEmpty(roleCfg.Provider, pName)
		roleProvider := buildProviderForSelection(cfg, roleProviderName, roleCfg.Model, roleCfg.BaseURL, roleCfg.APIKey)
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
