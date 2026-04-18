package app

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

// mockProvider is used when no LLM API key is configured.
type mockProvider struct{}

func (m *mockProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "Mock response: configure an API key in ~/.selfmind/config.yaml to enable LLM inference", nil
}

func (m *mockProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{
		Content: "Mock response: configure an API key in ~/.selfmind/config.yaml to enable LLM inference",
	}, nil
}

func (m *mockProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: "Mock response: configure an API key in ~/.selfmind/config.yaml to enable LLM inference"}
	close(ch)
	return ch, nil
}

func buildLLMProvider(cfg *config.Config) llm.Provider {
	pType := strings.ToLower(cfg.Agent.Provider)
	
	// 1. 如果显式指定了供应商，优先使用
	switch pType {
	case "anthropic":
		if cfg.Providers.AnthropicAPIKey != "" {
			ad := llm.NewAnthropicAdapter(cfg.Providers.AnthropicAPIKey)
			if cfg.Agent.Model != "" {
				ad.Model = cfg.Agent.Model
			}
			return ad
		}
	case "openai":
		if cfg.Providers.OpenAIAPIKey != "" {
			ad := llm.NewOpenAIAdapter(cfg.Providers.OpenAIAPIKey)
			if cfg.Agent.Model != "" {
				ad.Model = cfg.Agent.Model
			}
			return ad
		}
	case "openrouter":
		if cfg.Providers.OpenRouterAPIKey != "" {
			ad := llm.NewOpenRouterAdapter(cfg.Providers.OpenRouterAPIKey)
			if cfg.Agent.Model != "" {
				ad.Model = cfg.Agent.Model
			}
			return ad
		}
	case "gemini":
		if cfg.Providers.GeminiAPIKey != "" {
			ad := llm.NewGeminiAdapter(cfg.Providers.GeminiAPIKey)
			if cfg.Agent.Model != "" {
				ad.Model = cfg.Agent.Model
			}
			return ad
		}
	case "minimax":
		if cfg.Providers.MiniMaxAPIKey != "" {
			ad := llm.NewMiniMaxAdapter(cfg.Providers.MiniMaxAPIKey)
			if cfg.Agent.Model != "" {
				ad.Model = cfg.Agent.Model
			}
			return ad
		}
	}

	// 2. 检查自定义动态供应商
	for _, cp := range cfg.Providers.Custom {
		if strings.EqualFold(cp.Name, cfg.Agent.Provider) {
			return llm.NewGenericOpenAIAdapter(cp.Name, cp.BaseURL, cp.APIKey, cp.Model)
		}
	}

	// 3. 自动匹配可用供应商 (Fallback 逻辑)
	switch {
	case cfg.Providers.AnthropicAPIKey != "":
		return llm.NewAnthropicAdapter(cfg.Providers.AnthropicAPIKey)
	case cfg.Providers.GeminiAPIKey != "":
		return llm.NewGeminiAdapter(cfg.Providers.GeminiAPIKey)
	case cfg.Providers.OpenAIAPIKey != "":
		return llm.NewOpenAIAdapter(cfg.Providers.OpenAIAPIKey)
	case cfg.Providers.MiniMaxAPIKey != "":
		return llm.NewMiniMaxAdapter(cfg.Providers.MiniMaxAPIKey)
	case cfg.Providers.OpenRouterAPIKey != "":
		return llm.NewOpenRouterAdapter(cfg.Providers.OpenRouterAPIKey)
	default:
		println("[WARN] No LLM API key configured. Set anthropic_api_key, gemini_api_key, openai_api_key, minimax_api_key or openrouter_api_key in ~/.selfmind/config.yaml")
		return &mockProvider{}
	}
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

// InitAgent creates the LLM provider, reflection engine, and agent core.
func InitAgent(mem *memory.MemoryManager, cfg *config.Config) (*kernel.Agent, error) {
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
	tenantID := "user1" // 默认租户，后续可从 Context 获取
	pName := strings.ToLower(cfg.Agent.Provider)
	if pName == "" {
		// 回退探测逻辑
		if cfg.Providers.AnthropicAPIKey != "" {
			pName = "anthropic"
		} else if cfg.Providers.GeminiAPIKey != "" {
			pName = "gemini"
		} else if cfg.Providers.OpenAIAPIKey != "" {
			pName = "openai"
		}
	}

	// 注入 KeyGetter
	if pName != "" {
		getter := buildKeyGetter(mem, tenantID, pName)
		if a, ok := provider.(*llm.AnthropicAdapter); ok {
			a.KeyGetter = getter
		} else if o, ok := provider.(*llm.OpenAIAdapter); ok {
			o.KeyGetter = getter
		} else if g, ok := provider.(*llm.GeminiAdapter); ok {
			g.KeyGetter = getter
		} else if m, ok := provider.(*llm.MiniMaxAdapter); ok {
			m.KeyGetter = getter
		}
	}

	refl := &kernel.ReflectionEngine{
		Provider: provider,
		Config: kernel.EvolutionConfig{
			Enabled:                cfg.Evolution.Enabled,
			Mode:                   cfg.Evolution.Mode,
			MinComplexityThreshold: cfg.Evolution.MinComplexityThreshold,
			AutoArchiveConfidence:  cfg.Evolution.AutoArchiveConfidence,
		},
	}

	maxIter := cfg.Agent.MaxIterations
	if maxIter == 0 {
		maxIter = 90
	}
	maxRetries := cfg.Agent.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}

	agent := kernel.NewAgent(mem, nil, provider, cfg.Agent.Soul, maxIter, maxRetries, refl)
	return agent, nil
}
