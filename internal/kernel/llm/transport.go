package llm

import (
	"sort"
	"strings"
	"sync"
)

const (
	TransportOpenAIChat       = "openai_chat"
	TransportOpenAICompatible = "openai_compatible"
	TransportAnthropic        = "anthropic_messages"
	TransportResponses        = "codex_responses"
)

// TransportConfig is the app/runtime-to-LLM boundary. modelruntime resolves
// provider metadata and credentials; llm transports consume this protocol-level
// config without reaching back into app, gateway, or channel code.
type TransportConfig struct {
	Provider        string
	Protocol        string
	Model           string
	BaseURL         string
	APIKey          string
	KeyGetter       func() string
	TokenRefresher  func() string
	Headers         map[string]string
	ExtraBody       map[string]interface{}
	ExtraQuery      map[string]interface{}
	MaxTokens       int
	ReasoningEffort string
	Thinking        map[string]interface{}
	ServiceTier     string
	Quirks          ProviderQuirks

	// Responses-specific flags live on the transport config because they are
	// wire contract details, not general agent behavior.
	ResponsesStoreFalse    bool
	ResponsesRequireStream bool
}

type TransportFactory func(TransportConfig) Provider

var transportRegistry = struct {
	sync.RWMutex
	factories map[string]TransportFactory
}{factories: map[string]TransportFactory{}}

func init() {
	RegisterTransport(TransportAnthropic, buildAnthropicTransport)
	RegisterTransport(TransportResponses, buildResponsesTransport)
	RegisterTransport(TransportOpenAIChat, buildOpenAIChatTransport)
	RegisterTransport(TransportOpenAICompatible, buildOpenAICompatibleTransport)
}

// RegisterTransport registers one protocol family. Provider-specific behavior
// should be expressed through TransportConfig/ProviderQuirks before adding a
// new transport.
func RegisterTransport(protocol string, factory TransportFactory) {
	protocol = NormalizeTransportProtocol(protocol)
	if protocol == "" || factory == nil {
		return
	}
	transportRegistry.Lock()
	defer transportRegistry.Unlock()
	transportRegistry.factories[protocol] = factory
}

// BuildTransportProvider returns a concrete Provider for a resolved protocol.
// This is the only place that should choose adapter implementations.
func BuildTransportProvider(cfg TransportConfig) Provider {
	protocol := NormalizeTransportProtocol(cfg.Protocol)
	transportRegistry.RLock()
	factory := transportRegistry.factories[protocol]
	transportRegistry.RUnlock()
	if factory == nil {
		return nil
	}
	cfg.Protocol = protocol
	// MaybeWrapVCR is a no-op unless SELFMIND_EVAL_VCR is record|replay, so
	// production paths are untouched.
	return MaybeWrapVCR(factory(cfg))
}

func RegisteredTransports() []string {
	transportRegistry.RLock()
	defer transportRegistry.RUnlock()
	out := make([]string, 0, len(transportRegistry.factories))
	for protocol := range transportRegistry.factories {
		out = append(out, protocol)
	}
	sort.Strings(out)
	return out
}

func NormalizeTransportProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "openai", "openai-compatible", "openai_compatible", "chat_completions":
		return TransportOpenAICompatible
	case "openai_chat":
		return TransportOpenAIChat
	case "anthropic", "anthropic-messages", "anthropic_messages":
		return TransportAnthropic
	case "responses", "codex-responses", "codex_responses", "openai_responses":
		return TransportResponses
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func buildAnthropicTransport(cfg TransportConfig) Provider {
	ad := NewAnthropicAdapter(cfg.APIKey)
	ad.ProviderName = strings.TrimSpace(cfg.Provider)
	if strings.TrimSpace(cfg.Model) != "" {
		ad.Model = strings.TrimSpace(cfg.Model)
	}
	ad.BaseURL = anthropicMessagesTransportURL(cfg.BaseURL)
	ad.KeyGetter = cfg.KeyGetter
	ad.TokenRefresher = cfg.TokenRefresher
	ad.Headers = cfg.Headers
	ad.ExtraBody = cfg.ExtraBody
	ad.ExtraQuery = cfg.ExtraQuery
	ad.MaxTokens = firstPositiveTransport(cfg.MaxTokens, ad.MaxTokens)
	ad.ReasoningEffort = cfg.ReasoningEffort
	ad.Thinking = cfg.Thinking
	ad.ServiceTier = cfg.ServiceTier
	ad.Quirks = cfg.Quirks
	return ad
}

func buildResponsesTransport(cfg TransportConfig) Provider {
	ad := NewResponsesAdapter(cfg.APIKey, cfg.BaseURL, strings.TrimSpace(cfg.Model))
	ad.KeyGetter = cfg.KeyGetter
	ad.TokenRefresher = cfg.TokenRefresher
	ad.Headers = cfg.Headers
	ad.ExtraBody = cfg.ExtraBody
	ad.ExtraQuery = cfg.ExtraQuery
	ad.ReasoningEffort = cfg.ReasoningEffort
	ad.Quirks = cfg.Quirks
	if cfg.ResponsesStoreFalse {
		store := false
		ad.Store = &store
	}
	ad.RequireStream = cfg.ResponsesRequireStream
	return ad
}

func buildOpenAIChatTransport(cfg TransportConfig) Provider {
	ad := NewOpenAIAdapter(cfg.APIKey)
	applyOpenAITransportConfig(ad, cfg, chatCompletionsTransportURL(cfg.BaseURL))
	return ad
}

func buildOpenAICompatibleTransport(cfg TransportConfig) Provider {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch provider {
	case "openrouter":
		ad := NewOpenRouterAdapter(cfg.APIKey)
		applyOpenRouterTransportConfig(ad, cfg, chatCompletionsTransportURL(cfg.BaseURL))
		return ad
	case "google", "gemini", "gemini-cli":
		ad := NewGeminiAdapter(cfg.APIKey)
		applyOpenAITransportConfig(&ad.OpenAIAdapter, cfg, googleChatCompletionsTransportURL(cfg.BaseURL))
		return ad
	default:
		ad := NewGenericOpenAIAdapter(cfg.Provider, chatCompletionsTransportURL(cfg.BaseURL), cfg.APIKey, strings.TrimSpace(cfg.Model))
		applyOpenAITransportConfig(&ad.OpenAIAdapter, cfg, ad.BaseURL)
		return ad
	}
}

func applyOpenAITransportConfig(ad *OpenAIAdapter, cfg TransportConfig, baseURL string) {
	if ad == nil {
		return
	}
	if strings.TrimSpace(cfg.Model) != "" {
		ad.Model = strings.TrimSpace(cfg.Model)
	}
	ad.BaseURL = baseURL
	ad.KeyGetter = cfg.KeyGetter
	ad.TokenRefresher = cfg.TokenRefresher
	ad.Headers = cfg.Headers
	ad.ExtraBody = cfg.ExtraBody
	ad.ExtraQuery = cfg.ExtraQuery
	ad.MaxTokens = cfg.MaxTokens
	ad.ReasoningEffort = cfg.ReasoningEffort
	ad.Thinking = cfg.Thinking
	ad.ServiceTier = cfg.ServiceTier
	ad.Quirks = cfg.Quirks
}

func applyOpenRouterTransportConfig(ad *OpenRouterAdapter, cfg TransportConfig, baseURL string) {
	if ad == nil {
		return
	}
	if strings.TrimSpace(cfg.Model) != "" {
		ad.Model = strings.TrimSpace(cfg.Model)
	}
	ad.BaseURL = baseURL
	ad.KeyGetter = cfg.KeyGetter
	ad.TokenRefresher = cfg.TokenRefresher
	ad.Headers = cfg.Headers
	ad.ExtraBody = cfg.ExtraBody
	ad.ExtraQuery = cfg.ExtraQuery
	ad.MaxTokens = cfg.MaxTokens
	ad.ReasoningEffort = cfg.ReasoningEffort
	ad.Thinking = cfg.Thinking
	ad.ServiceTier = cfg.ServiceTier
	ad.Quirks = cfg.Quirks
}

func chatCompletionsTransportURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "https://api.openai.com/v1/chat/completions"
	}
	if strings.HasSuffix(strings.ToLower(baseURL), "/chat/completions") {
		return baseURL
	}
	return baseURL + "/chat/completions"
}

func anthropicMessagesTransportURL(baseURL string) string {
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

func googleChatCompletionsTransportURL(baseURL string) string {
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
	return chatCompletionsTransportURL(baseURL)
}

func firstPositiveTransport(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
