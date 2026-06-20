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
	AuthMiniMaxOAuth  = "oauth_minimax"
	AuthNone          = "none"
)

const (
	AuthHeaderXAPIKey = "x_api_key"
	AuthHeaderBearer  = "bearer"
	AuthHeaderAuto    = "auto"
)

const (
	ToolSchemaOpenAI    = "openai"
	ToolSchemaAnthropic = "anthropic"
	ToolSchemaMoonshot  = "moonshot"
)

const (
	SystemMessageTopLevel = "top_level"
	SystemMessageInline   = "inline"
)

const (
	ThinkingModeAnthropic = "anthropic"
	ThinkingModeKimi      = "kimi"
	ThinkingModeMiniMax   = "minimax"
	ThinkingModeOpenAI    = "openai"
	ThinkingModeOmit      = "omit"
)

// ProviderQuirks describes provider-specific wire behavior in declarative form.
// Transports consume these knobs instead of scattering provider-name conditionals.
type ProviderQuirks struct {
	AuthHeader        string
	ToolSchema        string
	SystemMessageMode string
	ThinkingMode      string
	UserAgent         string
	DisableHTTP2      bool
	SupportsTools     bool
	SupportsStreaming bool
	SupportsVision    bool
}

// ProviderProfile describes one model provider without owning credentials or
// client construction. The resolver combines this metadata with config and
// credential sources into a Runtime.
type ProviderProfile struct {
	ID              string
	DisplayName     string
	Aliases         []string
	Protocol        string
	AuthType        string
	BaseURL         string
	APIKeyEnvVars   []string
	BaseURLEnvVar   string
	ExternalSource  string
	ModelList       ModelListKind
	FallbackModels  []string
	ContextLength   int
	Headers         map[string]string
	MaxTokens       int
	ReasoningEffort string
	Thinking        map[string]interface{}
	ServiceTier     string
	Quirks          ProviderQuirks
}

type ModelListKind string

const (
	ModelListOpenAICompatible ModelListKind = "openai_compatible"
	ModelListAnthropic        ModelListKind = "anthropic"
	ModelListGoogle           ModelListKind = "google"
	ModelListCodex            ModelListKind = "codex"
	ModelListStatic           ModelListKind = "static"
)

func openAIQuirks() ProviderQuirks {
	return ProviderQuirks{
		AuthHeader:        AuthHeaderBearer,
		ToolSchema:        ToolSchemaOpenAI,
		SystemMessageMode: SystemMessageInline,
		ThinkingMode:      ThinkingModeOpenAI,
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    true,
	}
}

func anthropicQuirks() ProviderQuirks {
	return ProviderQuirks{
		AuthHeader:        AuthHeaderXAPIKey,
		ToolSchema:        ToolSchemaAnthropic,
		SystemMessageMode: SystemMessageTopLevel,
		ThinkingMode:      ThinkingModeAnthropic,
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    true,
	}
}

func minimaxQuirks() ProviderQuirks {
	q := anthropicQuirks()
	q.AuthHeader = AuthHeaderBearer
	q.ThinkingMode = ThinkingModeMiniMax
	return q
}

func kimiQuirks() ProviderQuirks {
	q := anthropicQuirks()
	q.ToolSchema = ToolSchemaMoonshot
	q.ThinkingMode = ThinkingModeKimi
	q.UserAgent = "claude-code/0.1.0"
	// Hermes reaches api.kimi.com/coding through httpx/Anthropic SDK's
	// HTTP/1.1 path. Go's default client eagerly negotiates HTTP/2, which this
	// endpoint can close with unexpected EOF on POST/stream requests.
	q.DisableHTTP2 = true
	q.SupportsVision = false
	return q
}

func BuiltinProfiles() []ProviderProfile {
	return []ProviderProfile{
		{
			ID: "openai", DisplayName: "OpenAI", Aliases: []string{"openai-api"},
			Protocol: ProtocolOpenAIChat, AuthType: AuthAPIKey,
			BaseURL:       "https://api.openai.com/v1",
			APIKeyEnvVars: []string{"OPENAI_API_KEY"}, BaseURLEnvVar: "OPENAI_BASE_URL",
			ModelList:      ModelListOpenAICompatible,
			FallbackModels: []string{"gpt-4o", "gpt-4o-mini"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "anthropic", DisplayName: "Anthropic", Aliases: []string{"claude"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL:       "https://api.anthropic.com",
			APIKeyEnvVars: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"},
			BaseURLEnvVar: "ANTHROPIC_BASE_URL", ExternalSource: "claude-code",
			ModelList:      ModelListAnthropic,
			FallbackModels: []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"},
			Quirks:         anthropicQuirks(),
		},
		{
			ID: "claude-code", DisplayName: "Claude Code", Aliases: []string{"claude-oauth"},
			Protocol: ProtocolAnthropic, AuthType: AuthExternalOAuth,
			BaseURL: "https://api.anthropic.com", ExternalSource: "claude-code",
			ModelList:      ModelListAnthropic,
			FallbackModels: []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"},
			Quirks:         anthropicQuirks(),
		},
		{
			ID: "google", DisplayName: "Google AI Studio", Aliases: []string{"gemini"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL:       "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			APIKeyEnvVars: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, BaseURLEnvVar: "GEMINI_BASE_URL",
			ModelList:      ModelListGoogle,
			FallbackModels: []string{"gemini-1.5-pro", "gemini-1.5-flash"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "gemini-cli", DisplayName: "Gemini CLI", Aliases: []string{"google-gemini-cli", "gemini-oauth"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthExternalOAuth,
			BaseURL:        "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			ExternalSource: "gemini-cli", ModelList: ModelListGoogle,
			FallbackModels: []string{"gemini-1.5-pro", "gemini-1.5-flash"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "qwen-cli", DisplayName: "Qwen CLI", Aliases: []string{"qwen-oauth", "qwen-portal"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthExternalOAuth,
			BaseURL: "https://portal.qwen.ai/v1", ExternalSource: "qwen-cli",
			ModelList:      ModelListOpenAICompatible,
			FallbackModels: []string{"qwen3-coder-plus", "qwen-plus", "qwen-max"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "codex-cli", DisplayName: "Codex CLI", Aliases: []string{"openai-codex", "codex"},
			Protocol: ProtocolResponses, AuthType: AuthExternalOAuth,
			BaseURL: "https://chatgpt.com/backend-api/codex", ExternalSource: "codex-cli",
			ModelList:      ModelListCodex,
			FallbackModels: []string{"gpt-5.5", "gpt-5.3-codex"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "openrouter", DisplayName: "OpenRouter",
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://openrouter.ai/api/v1", APIKeyEnvVars: []string{"OPENROUTER_API_KEY"},
			BaseURLEnvVar: "OPENROUTER_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"anthropic/claude-3.5-sonnet", "openai/gpt-4o"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "minimax", DisplayName: "MiniMax", Aliases: []string{"mini-max"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.minimax.io/anthropic", APIKeyEnvVars: []string{"MINIMAX_API_KEY"},
			BaseURLEnvVar: "MINIMAX_BASE_URL", ModelList: ModelListAnthropic,
			FallbackModels: []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5"},
			ContextLength:  204800,
			MaxTokens:      32768,
			Quirks:         minimaxQuirks(),
		},
		{
			ID: "minimax-cn", DisplayName: "MiniMax (China)", Aliases: []string{"minimax-china", "minimax_cn"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.minimaxi.com/anthropic", APIKeyEnvVars: []string{"MINIMAX_CN_API_KEY"},
			BaseURLEnvVar: "MINIMAX_CN_BASE_URL", ModelList: ModelListAnthropic,
			FallbackModels: []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5"},
			ContextLength:  204800,
			MaxTokens:      32768,
			Quirks:         minimaxQuirks(),
		},
		{
			ID: "minimax-oauth", DisplayName: "MiniMax OAuth", Aliases: []string{"minimax_oauth", "minimax-portal", "minimax-global"},
			Protocol: ProtocolAnthropic, AuthType: AuthMiniMaxOAuth,
			BaseURL: "https://api.minimax.io/anthropic", ModelList: ModelListAnthropic,
			FallbackModels: []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5"},
			ContextLength:  204800,
			MaxTokens:      32768,
			Quirks:         minimaxQuirks(),
		},
		{
			ID: "kimi-coding", DisplayName: "Kimi Coding Plan", Aliases: []string{"kimi", "moonshot", "kimi-for-coding"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.kimi.com/coding", APIKeyEnvVars: []string{"KIMI_CODING_API_KEY", "KIMI_API_KEY"},
			BaseURLEnvVar: "KIMI_BASE_URL", ModelList: ModelListStatic,
			FallbackModels:  []string{"kimi-for-coding"},
			ContextLength:   262144,
			Headers:         map[string]string{"User-Agent": "claude-code/0.1.0"},
			MaxTokens:       32000,
			ReasoningEffort: "medium",
			Thinking:        map[string]interface{}{"type": "enabled"},
			Quirks:          kimiQuirks(),
		},
		{
			ID: "deepseek", DisplayName: "DeepSeek",
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://api.deepseek.com/v1", APIKeyEnvVars: []string{"DEEPSEEK_API_KEY"},
			BaseURLEnvVar: "DEEPSEEK_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"deepseek-chat", "deepseek-reasoner"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "zai", DisplayName: "Z.AI / GLM", Aliases: []string{"glm", "z-ai", "zhipu"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://api.z.ai/api/paas/v4", APIKeyEnvVars: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"},
			BaseURLEnvVar: "GLM_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"glm-4.5", "glm-4.5-flash"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "alibaba-coding-plan", DisplayName: "Alibaba Coding Plan", Aliases: []string{"dashscope-coding", "qwen-coding"},
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://coding-intl.dashscope.aliyuncs.com/v1", APIKeyEnvVars: []string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY"},
			BaseURLEnvVar: "ALIBABA_CODING_PLAN_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels: []string{"qwen3-coder-plus", "qwen-plus", "kimi-k2.5"},
			Quirks:         openAIQuirks(),
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
