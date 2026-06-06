package modelruntime

import (
	"sort"
	"strings"
)

const (
	ProtocolOpenAIChat       = "openai_chat"
	ProtocolOpenAICompatible = "openai_compatible"
	ProtocolAnthropic        = "anthropic_messages"
	ProtocolResponses        = "codex_responses"
)

const (
	AuthAPIKey        = "api_key"
	AuthExternalOAuth = "external_oauth"
	AuthNone          = "none"
)

// ProviderProfile describes one model provider without owning credentials or
// client construction. The resolver combines this metadata with config and
// credential sources into a Runtime.
type ProviderProfile struct {
	ID             string
	DisplayName    string
	Aliases        []string
	Protocol       string
	AuthType       string
	BaseURL        string
	APIKeyEnvVars  []string
	BaseURLEnvVar  string
	ExternalSource string
	ModelList      ModelListKind
	FallbackModels []string
}

type ModelListKind string

const (
	ModelListOpenAICompatible ModelListKind = "openai_compatible"
	ModelListAnthropic        ModelListKind = "anthropic"
	ModelListGoogle           ModelListKind = "google"
	ModelListCodex            ModelListKind = "codex"
	ModelListStatic           ModelListKind = "static"
)

func BuiltinProfiles() []ProviderProfile {
	return []ProviderProfile{
		{
			ID: "openai", DisplayName: "OpenAI", Aliases: []string{"openai-api"},
			Protocol: ProtocolOpenAIChat, AuthType: AuthAPIKey,
			BaseURL:       "https://api.openai.com/v1",
			APIKeyEnvVars: []string{"OPENAI_API_KEY"}, BaseURLEnvVar: "OPENAI_BASE_URL",
			ModelList:      ModelListOpenAICompatible,
			FallbackModels: []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			ID: "anthropic", DisplayName: "Anthropic", Aliases: []string{"claude"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL:       "https://api.anthropic.com",
			APIKeyEnvVars: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"},
			BaseURLEnvVar: "ANTHROPIC_BASE_URL", ExternalSource: "claude-code",
			ModelList:      ModelListAnthropic,
			FallbackModels: []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"},
		},
		{
			ID: "claude-code", DisplayName: "Claude Code", Aliases: []string{"claude-oauth"},
			Protocol: ProtocolAnthropic, AuthType: AuthExternalOAuth,
			BaseURL: "https://api.anthropic.com", ExternalSource: "claude-code",
			ModelList:      ModelListAnthropic,
			FallbackModels: []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"},
		},
		{
			ID: "google", DisplayName: "Google AI Studio", Aliases: []string{"gemini"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL:       "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			APIKeyEnvVars: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, BaseURLEnvVar: "GEMINI_BASE_URL",
			ModelList:      ModelListGoogle,
			FallbackModels: []string{"gemini-1.5-pro", "gemini-1.5-flash"},
		},
		{
			ID: "gemini-cli", DisplayName: "Gemini CLI", Aliases: []string{"google-gemini-cli", "gemini-oauth"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthExternalOAuth,
			BaseURL:        "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			ExternalSource: "gemini-cli", ModelList: ModelListGoogle,
			FallbackModels: []string{"gemini-1.5-pro", "gemini-1.5-flash"},
		},
		{
			ID: "qwen-cli", DisplayName: "Qwen CLI", Aliases: []string{"qwen-oauth", "qwen-portal"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthExternalOAuth,
			BaseURL: "https://portal.qwen.ai/v1", ExternalSource: "qwen-cli",
			ModelList:      ModelListOpenAICompatible,
			FallbackModels: []string{"qwen3-coder-plus", "qwen-plus", "qwen-max"},
		},
		{
			ID: "codex-cli", DisplayName: "Codex CLI", Aliases: []string{"openai-codex", "codex"},
			Protocol: ProtocolResponses, AuthType: AuthExternalOAuth,
			BaseURL: "https://chatgpt.com/backend-api/codex", ExternalSource: "codex-cli",
			ModelList:      ModelListCodex,
			FallbackModels: []string{"gpt-5.5", "gpt-5.3-codex"},
		},
		{
			ID: "openrouter", DisplayName: "OpenRouter",
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://openrouter.ai/api/v1", APIKeyEnvVars: []string{"OPENROUTER_API_KEY"},
			BaseURLEnvVar: "OPENROUTER_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"anthropic/claude-3.5-sonnet", "openai/gpt-4o"},
		},
		{
			ID: "minimax", DisplayName: "MiniMax", Aliases: []string{"mini-max"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.minimax.io/anthropic", APIKeyEnvVars: []string{"MINIMAX_API_KEY"},
			BaseURLEnvVar: "MINIMAX_BASE_URL", ModelList: ModelListAnthropic,
			FallbackModels: []string{"MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5"},
		},
		{
			ID: "kimi-coding", DisplayName: "Kimi Coding Plan", Aliases: []string{"kimi", "moonshot"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://api.kimi.com/coding/v1", APIKeyEnvVars: []string{"KIMI_CODING_API_KEY", "KIMI_API_KEY"},
			BaseURLEnvVar: "KIMI_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"kimi-for-coding"},
		},
		{
			ID: "deepseek", DisplayName: "DeepSeek",
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://api.deepseek.com/v1", APIKeyEnvVars: []string{"DEEPSEEK_API_KEY"},
			BaseURLEnvVar: "DEEPSEEK_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"deepseek-chat", "deepseek-reasoner"},
		},
		{
			ID: "zai", DisplayName: "Z.AI / GLM", Aliases: []string{"glm", "z-ai", "zhipu"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://api.z.ai/api/paas/v4", APIKeyEnvVars: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"},
			BaseURLEnvVar: "GLM_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"glm-4.5", "glm-4.5-flash"},
		},
		{
			ID: "alibaba-coding-plan", DisplayName: "Alibaba Coding Plan", Aliases: []string{"dashscope-coding", "qwen-coding"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://coding-intl.dashscope.aliyuncs.com/v1", APIKeyEnvVars: []string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY"},
			BaseURLEnvVar: "ALIBABA_CODING_PLAN_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"qwen3-coder-plus", "qwen-plus", "kimi-k2.5"},
		},
	}
}

type Registry struct {
	byID    map[string]ProviderProfile
	aliases map[string]string
}

func NewRegistry() *Registry {
	r := &Registry{byID: make(map[string]ProviderProfile), aliases: make(map[string]string)}
	for _, p := range BuiltinProfiles() {
		r.Register(p)
	}
	return r
}

func (r *Registry) Register(profile ProviderProfile) {
	id := NormalizeProviderID(profile.ID)
	if id == "" {
		return
	}
	profile.ID = id
	if profile.Protocol == "" {
		profile.Protocol = ProtocolOpenAICompatible
	}
	if profile.AuthType == "" {
		profile.AuthType = AuthAPIKey
	}
	r.byID[id] = profile
	r.aliases[id] = id
	for _, alias := range profile.Aliases {
		if key := NormalizeProviderID(alias); key != "" {
			r.aliases[key] = id
		}
	}
}

func (r *Registry) Resolve(name string) (ProviderProfile, bool) {
	key := NormalizeProviderID(name)
	if key == "" {
		return ProviderProfile{}, false
	}
	if id, ok := r.aliases[key]; ok {
		p, found := r.byID[id]
		return p, found
	}
	p, ok := r.byID[key]
	return p, ok
}

func (r *Registry) Profiles() []ProviderProfile {
	out := make([]ProviderProfile, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DisplayName < out[j].DisplayName
	})
	return out
}

func NormalizeProviderID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	return value
}

func NormalizeProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "openai", "openai-compatible", "openai_compatible", "chat_completions":
		return ProtocolOpenAICompatible
	case "openai_chat":
		return ProtocolOpenAIChat
	case "anthropic", "anthropic-messages", "anthropic_messages":
		return ProtocolAnthropic
	case "responses", "codex-responses", "codex_responses", "openai_responses":
		return ProtocolResponses
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}
