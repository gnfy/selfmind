package modelruntime

import (
	"fmt"
	"sort"
	"strings"

	"selfmind/internal/buildinfo"
)

const (
	ProtocolOpenAIChat       = "openai_chat"
	ProtocolOpenAICompatible = "openai_compatible"
	ProtocolAnthropic        = "anthropic_messages"
	ProtocolResponses        = "codex_responses"
)

const (
	UserIdentityAuto      = "auto"
	UserIdentityOpenAI    = "user_id"
	UserIdentityAnthropic = "metadata.user_id"
	UserIdentityOff       = "off"
	HTTPVersionAuto       = "auto"
	HTTPVersion1          = "http1"
	HTTPVersion2          = "http2"
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
	ThinkingModeDeepSeek  = "deepseek"
	ThinkingModeOmit      = "omit"
)

// ProviderQuirks describes provider-specific wire behavior in declarative form.
// Transports consume these knobs instead of scattering provider-name conditionals.
type ProviderQuirks struct {
	AuthHeader                string
	ToolSchema                string
	SystemMessageMode         string
	ThinkingMode              string
	UserIdentityField         string
	UserAgent                 string
	HTTPVersion               string
	DisableHTTP2              bool
	PromptCache               bool
	PromptCacheSet            bool
	ResponsesStoreFalse       bool
	ResponsesStoreFalseSet    bool
	ResponsesRequireStream    bool
	ResponsesRequireStreamSet bool
	SupportsTools             bool
	SupportsStreaming         bool
	SupportsVision            bool
}

// ValidateProviderQuirks rejects misspelled compatibility values before an
// adapter can silently ignore them. Protocol applicability is reported by
// QuirkDiagnostics because existing custom endpoints may intentionally use a
// provider-specific extension on a compatible wire protocol.
func ValidateProviderQuirks(q ProviderQuirks) error {
	if !oneOf(q.AuthHeader, "", AuthHeaderAuto, AuthHeaderBearer, AuthHeaderXAPIKey, "x-api-key") {
		return fmt.Errorf("unsupported auth_header quirk %q", q.AuthHeader)
	}
	if !oneOf(q.ToolSchema, "", ToolSchemaOpenAI, ToolSchemaAnthropic, ToolSchemaMoonshot) {
		return fmt.Errorf("unsupported tool_schema quirk %q", q.ToolSchema)
	}
	if !oneOf(q.ThinkingMode, "", ThinkingModeAnthropic, ThinkingModeKimi, ThinkingModeMiniMax, ThinkingModeOpenAI, ThinkingModeDeepSeek, ThinkingModeOmit) {
		return fmt.Errorf("unsupported thinking_mode quirk %q", q.ThinkingMode)
	}
	if !oneOf(q.UserIdentityField, "", UserIdentityAuto, UserIdentityOpenAI, UserIdentityAnthropic, UserIdentityOff) {
		return fmt.Errorf("unsupported user_identity_field quirk %q", q.UserIdentityField)
	}
	if !oneOf(q.HTTPVersion, "", HTTPVersionAuto, HTTPVersion1, HTTPVersion2) {
		return fmt.Errorf("unsupported http_version quirk %q", q.HTTPVersion)
	}
	return nil
}

// QuirkDiagnostics explains compatibility settings that are accepted for a
// migration window but do not apply to the selected wire protocol.
func QuirkDiagnostics(protocol string, q ProviderQuirks) []string {
	protocol = NormalizeProtocol(protocol)
	var out []string
	if strings.TrimSpace(q.SystemMessageMode) != "" {
		out = append(out, "system_message_mode is deprecated and ignored; protocol adapters own system-message placement")
	}
	if q.PromptCache && protocol != ProtocolAnthropic {
		out = append(out, "prompt_cache only controls Anthropic cache_control; this protocol may still provide automatic server-side caching")
	}
	if (q.ResponsesStoreFalse || q.ResponsesRequireStream) && protocol != ProtocolResponses {
		out = append(out, "responses_* quirks are ignored outside the Responses protocol")
	}
	if q.UserIdentityField == UserIdentityAnthropic && protocol != ProtocolAnthropic {
		out = append(out, "metadata.user_id is only emitted by the Anthropic Messages protocol")
	}
	if q.UserIdentityField == UserIdentityOpenAI && protocol == ProtocolAnthropic {
		out = append(out, "user_id is an OpenAI-compatible field; use auto or metadata.user_id for Anthropic Messages")
	}
	return out
}

func oneOf(value string, values ...string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range values {
		if value == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
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

// minimaxFallbackModels is shared by the minimax, minimax-cn, and
// minimax-oauth profiles so the fallback list stays consistent across them.
var minimaxFallbackModels = []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5"}

func openAIQuirks() ProviderQuirks {
	return ProviderQuirks{
		AuthHeader:        AuthHeaderBearer,
		ToolSchema:        ToolSchemaOpenAI,
		ThinkingMode:      ThinkingModeOpenAI,
		HTTPVersion:       "auto",
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    true,
	}
}

func anthropicQuirks() ProviderQuirks {
	return ProviderQuirks{
		AuthHeader:        AuthHeaderXAPIKey,
		ToolSchema:        ToolSchemaAnthropic,
		ThinkingMode:      ThinkingModeAnthropic,
		HTTPVersion:       "auto",
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    true,
	}
}

func cachedAnthropicQuirks() ProviderQuirks {
	q := anthropicQuirks()
	q.PromptCache = true
	return q
}

func minimaxQuirks() ProviderQuirks {
	q := cachedAnthropicQuirks()
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
	q.HTTPVersion = "http1"
	q.SupportsVision = false
	return q
}

func deepSeekQuirks() ProviderQuirks {
	q := openAIQuirks()
	q.ThinkingMode = ThinkingModeDeepSeek
	q.UserIdentityField = "user_id"
	return q
}

func codexResponsesQuirks() ProviderQuirks {
	q := openAIQuirks()
	q.ResponsesStoreFalse = true
	q.ResponsesRequireStream = true
	return q
}

// openRouterAttributionHeaders identifies this app to OpenRouter: HTTP-Referer
// becomes the app link and X-Title the display name in its app attribution.
// The headers belong to the profile rather than to one adapter so every
// protocol carries them — a profile configured as `openai_chat`, or any
// streaming call, never reaches the OpenRouter adapter's own request builder.
// Being a profile layer also keeps `provider_profiles.openrouter.extra_headers`
// able to override them.
func openRouterAttributionHeaders() map[string]string {
	return map[string]string{
		"HTTP-Referer": "https://github.com/gnfy/selfmind",
		"X-Title":      "SelfMind Agent",
		"User-Agent":   selfmindUserAgent(),
	}
}

// selfmindUserAgent tracks the running build instead of a pinned string, so a
// provider-side breakdown by version stays truthful after an upgrade. It
// mirrors the update checker's spelling.
func selfmindUserAgent() string {
	version := strings.TrimPrefix(strings.TrimSpace(buildinfo.Version), "v")
	if version == "" {
		version = "dev"
	}
	return "selfmind/" + version
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
			Quirks:         cachedAnthropicQuirks(),
		},
		{
			ID: "claude-code", DisplayName: "Claude Code", Aliases: []string{"claude-oauth"},
			Protocol: ProtocolAnthropic, AuthType: AuthExternalOAuth,
			BaseURL: "https://api.anthropic.com", ExternalSource: "claude-code",
			ModelList:      ModelListAnthropic,
			FallbackModels: []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"},
			Quirks:         cachedAnthropicQuirks(),
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
			Headers: map[string]string{
				"originator":  "codex_cli_rs",
				"OpenAI-Beta": "responses=experimental",
				"User-Agent":  "codex_cli_rs/0.142.2",
			},
			ModelList:      ModelListCodex,
			FallbackModels: []string{"gpt-5.5", "gpt-5.3-codex"},
			// Omit reasoning by default so the selected model/provider owns its
			// default. Explicit models.primary.reasoning is still forwarded.
			Quirks: codexResponsesQuirks(),
		},
		{
			ID: "openrouter", DisplayName: "OpenRouter",
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://openrouter.ai/api/v1", APIKeyEnvVars: []string{"OPENROUTER_API_KEY"},
			BaseURLEnvVar: "OPENROUTER_BASE_URL", ModelList: ModelListOpenAICompatible,
			Headers:        openRouterAttributionHeaders(),
			FallbackModels: []string{"anthropic/claude-3.5-sonnet", "openai/gpt-4o"},
			Quirks:         openAIQuirks(),
		},
		{
			ID: "minimax", DisplayName: "MiniMax", Aliases: []string{"mini-max"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.minimax.io/anthropic", APIKeyEnvVars: []string{"MINIMAX_API_KEY"},
			BaseURLEnvVar: "MINIMAX_BASE_URL", ModelList: ModelListAnthropic,
			FallbackModels: minimaxFallbackModels,
			ContextLength:  204800,
			MaxTokens:      32768,
			Quirks:         minimaxQuirks(),
		},
		{
			ID: "minimax-cn", DisplayName: "MiniMax (China)", Aliases: []string{"minimax-china", "minimax_cn"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.minimaxi.com/anthropic", APIKeyEnvVars: []string{"MINIMAX_CN_API_KEY"},
			BaseURLEnvVar: "MINIMAX_CN_BASE_URL", ModelList: ModelListAnthropic,
			FallbackModels: minimaxFallbackModels,
			ContextLength:  204800,
			MaxTokens:      32768,
			Quirks:         minimaxQuirks(),
		},
		{
			ID: "minimax-oauth", DisplayName: "MiniMax OAuth", Aliases: []string{"minimax_oauth", "minimax-portal", "minimax-global"},
			Protocol: ProtocolAnthropic, AuthType: AuthMiniMaxOAuth,
			BaseURL: "https://api.minimax.io/anthropic", ModelList: ModelListAnthropic,
			FallbackModels: minimaxFallbackModels,
			ContextLength:  204800,
			MaxTokens:      32768,
			Quirks:         minimaxQuirks(),
		},
		{
			ID: "kimi-coding", DisplayName: "Kimi Coding Plan", Aliases: []string{"kimi", "moonshot", "kimi-for-coding"},
			Protocol: ProtocolAnthropic, AuthType: AuthAPIKey,
			BaseURL: "https://api.kimi.com/coding", APIKeyEnvVars: []string{"KIMI_CODING_API_KEY", "KIMI_API_KEY"},
			BaseURLEnvVar: "KIMI_BASE_URL", ModelList: ModelListStatic,
			FallbackModels: []string{"kimi-for-coding", "kimi-for-coding-highspeed"},
			ContextLength:  262144,
			Headers:        map[string]string{"User-Agent": "claude-code/0.1.0"},
			MaxTokens:      32000,
			Thinking:       map[string]interface{}{"type": "enabled"},
			Quirks:         kimiQuirks(),
		},
		{
			ID: "deepseek", DisplayName: "DeepSeek",
			Protocol: ProtocolOpenAICompatible, AuthType: AuthAPIKey,
			BaseURL: "https://api.deepseek.com/v1", APIKeyEnvVars: []string{"DEEPSEEK_API_KEY"},
			BaseURLEnvVar: "DEEPSEEK_BASE_URL", ModelList: ModelListOpenAICompatible,
			FallbackModels:  []string{"deepseek-v4-flash", "deepseek-v4-pro"},
			ReasoningEffort: "high",
			Thinking:        map[string]interface{}{"type": "enabled"},
			Quirks:          deepSeekQuirks(),
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
