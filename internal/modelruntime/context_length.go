package modelruntime

import "strings"

const DefaultFallbackContextLength = 256000

var defaultContextLengths = []struct {
	match string
	value int
}{
	// Specific entries must stay before broader family entries.
	{"claude-opus-4-8", 1000000},
	{"claude-opus-4.8", 1000000},
	{"claude-opus-4-7", 1000000},
	{"claude-opus-4.7", 1000000},
	{"claude-opus-4-6", 1000000},
	{"claude-sonnet-4-6", 1000000},
	{"claude-opus-4.6", 1000000},
	{"claude-sonnet-4.6", 1000000},
	{"claude", 200000},

	{"gpt-5.5", 1050000},
	{"gpt-5.4-nano", 400000},
	{"gpt-5.4-mini", 400000},
	{"gpt-5.4", 1050000},
	{"gpt-5.3-codex-spark", 128000},
	{"gpt-5.1-chat", 128000},
	{"gpt-5", 400000},
	{"gpt-4.1", 1047576},
	{"gpt-4", 128000},

	{"gemini", 1048576},
	{"gemma-4-31b", 256000},
	{"gemma-4", 256000},
	{"gemma4", 256000},
	{"gemma-3", 131072},
	{"gemma", 8192},

	{"deepseek-v4-pro", 1000000},
	{"deepseek-v4-flash", 1000000},
	{"deepseek-chat", 1000000},
	{"deepseek-reasoner", 1000000},
	{"deepseek", 128000},

	{"qwen3.6-plus", 1048576},
	{"qwen3-coder-plus", 1000000},
	{"qwen3-coder", 262144},
	{"qwen", 131072},

	{"minimax", 204800},
	{"kimi", 262144},
	{"moonshotai/kimi-k2", 262144},
	{"moonshot", 262144},

	{"glm", 202752},
	{"grok-4-fast", 2000000},
	{"grok-4.20", 2000000},
	{"grok-4.3", 1000000},
	{"grok-4", 256000},
	{"grok-3", 131072},
	{"grok-2", 131072},
	{"grok", 131072},

	{"llama", 131072},
}

// KnownContextLength returns a best-effort total context window for display
// and budgeting. Explicit config should be preferred by callers; this function
// is only the built-in fallback table.
func KnownContextLength(provider, model string) int {
	provider = NormalizeProviderID(provider)
	model = strings.ToLower(strings.TrimSpace(stripProviderPrefix(model)))
	switch provider {
	case "kimi-coding":
		return 262144
	case "minimax", "minimax-cn", "minimax-oauth":
		return 204800
	case "anthropic", "claude-code":
		if model == "" {
			return 200000
		}
	case "google", "gemini-cli":
		if model == "" {
			return 1048576
		}
	}
	if model == "" {
		return 0
	}
	for _, item := range defaultContextLengths {
		if strings.Contains(model, item.match) {
			return item.value
		}
	}
	return 0
}

func stripProviderPrefix(model string) string {
	if !strings.Contains(model, ":") || strings.HasPrefix(model, "http") {
		return model
	}
	prefix, suffix, ok := strings.Cut(model, ":")
	if !ok {
		return model
	}
	switch NormalizeProviderID(prefix) {
	case "openrouter", "openai", "anthropic", "claude", "google", "gemini",
		"kimi", "kimi-coding", "moonshot", "minimax", "minimax-oauth",
		"minimax-cn", "deepseek", "qwen", "dashscope", "zai", "glm",
		"custom", "local":
		return suffix
	default:
		return model
	}
}
