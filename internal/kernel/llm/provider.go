package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ChatRequest is the unified request shape for model calls.
type ChatRequest struct {
	Model          string
	Messages       []Message
	Tools          []ToolDefinition
	MaxTokens      int
	SystemPrompt   string
	PromptCacheKey string
	Options        map[string]interface{}
}

// reasoningDisabled is the protocol-neutral spelling used by bounded control
// and maintenance calls. Adapters translate it to the vendor's disabled form
// (or omit the reasoning field) instead of forwarding an unsupported literal.
func reasoningDisabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "off", "disabled":
		return true
	default:
		return false
	}
}

// Message is one conversation entry.
type Message struct {
	Role             string
	Content          string
	ReasoningContent string
	MultiContent     []ContentPart
	Name             string
	ToolCallID       string
	ToolCalls        []ToolCall
}

// ContentPart is one multimodal content block.
type ContentPart struct {
	Type     string // "text", "image_url", "image_base64"
	Text     string
	ImageURL string
	MimeType string
	Data     string // Base64 encoded data
}

// ToolDefinition describes a tool the LLM may call.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

type ProviderQuirks struct {
	AuthHeader        string
	ToolSchema        string
	SystemMessageMode string
	ThinkingMode      string
	UserIdentityField string
	UserAgent         string
	HTTPVersion       string
	DisableHTTP2      bool
	// PromptCache opts a provider into explicit prompt-cache breakpoints
	// (Anthropic cache_control). Off by default: request bytes must be
	// unchanged unless a profile or selection enables it.
	PromptCache       bool
	SupportsTools     bool
	SupportsStreaming bool
	SupportsVision    bool
}

// ChatResponse is the unified response shape.
type ChatResponse struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	Usage            UsageStats
	FinishReason     string
}

type ToolCall struct {
	ID       string
	Function string
	Args     string
}

type UsageStats struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Prompt-cache accounting. InputTokens stays the logical input total;
	// CacheMissInputTokens is the full-price input when the provider reports it.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheMissInputTokens     int `json:"cache_miss_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens"`
	// CacheUsageReported distinguishes a real zero hit from a transport that
	// does not expose cache read/miss accounting.
	CacheUsageReported bool `json:"cache_usage_reported,omitempty"`
	// CacheCreationReported distinguishes a real zero from a transport that
	// does not expose cache-write accounting (for example Responses API).
	CacheCreationReported bool `json:"cache_creation_reported,omitempty"`
}

// StreamEvent is one streaming response event.
type StreamEvent struct {
	EventID          string
	Cursor           int64
	LiveSeq          uint64
	TaskID           string
	RunID            string
	Durability       string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	Usage            *UsageStats
	FinishReason     string
	Err              error
	EventType        string
	ToolName         string
	ToolCallID       string
	ToolArgs         string
	ToolResult       string
	DurationSeconds  float64
	Payload          map[string]interface{}
}

// Provider is the LLM invocation interface.
type Provider interface {
	ChatCompletion(ctx context.Context, messages []Message) (string, error)
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}

// RequestFingerprint describes the provider-adapter request shape without
// retaining prompt text, tool schemas, headers, or credentials. PrefixHash is
// built from the cache-relevant stable blocks; RequestHash covers the complete
// serialized request so diagnostics can distinguish prefix drift from ordinary
// conversation growth.
type RequestFingerprint struct {
	Protocol    string            `json:"protocol"`
	PrefixHash  string            `json:"prefix_hash"`
	RequestHash string            `json:"request_hash"`
	Blocks      map[string]string `json:"blocks,omitempty"`
}

// RequestFingerprinter is implemented by protocol adapters that can inspect
// their final wire shape. Wrappers must forward it to the provider selected for
// the current role.
type RequestFingerprinter interface {
	FingerprintRequest(context.Context, ChatRequest, bool) (RequestFingerprint, bool)
}

// FingerprintProviderRequest probes the optional adapter capability.
func FingerprintProviderRequest(ctx context.Context, p Provider, req ChatRequest, stream bool) (RequestFingerprint, bool) {
	if p == nil {
		return RequestFingerprint{}, false
	}
	if fingerprinter, ok := p.(RequestFingerprinter); ok {
		return fingerprinter.FingerprintRequest(ctx, req, stream)
	}
	return RequestFingerprint{}, false
}

func requestValueHash(value interface{}) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:8])
}

// NativeToolsCapable is an optional Provider capability probe: a provider that
// KNOWS its wire protocol carries native tool definitions (ChatRequest.Tools →
// the vendor's tools param) reports it here. buildSystemPrompt uses the probe
// to stop double-sending tool definitions (native param AND full text schemas
// in the system prompt). Wrapper providers (role router, VCR) must forward the
// probe to the provider they resolve to. A provider that does not implement
// the interface is treated as NOT native — the safe direction: unknown
// providers keep the full text fallback instead of losing tool definitions.
type NativeToolsCapable interface {
	SupportsNativeTools() bool
}

// ProviderSupportsNativeTools probes p, defaulting to false (keep text).
func ProviderSupportsNativeTools(p Provider) bool {
	if c, ok := p.(NativeToolsCapable); ok {
		return c.SupportsNativeTools()
	}
	return false
}
