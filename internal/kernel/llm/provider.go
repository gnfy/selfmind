package llm

import (
	"context"
)

// ChatRequest 是模型调用的统一标准结构
type ChatRequest struct {
	Model        string
	Messages     []Message
	Tools        []ToolDefinition
	MaxTokens    int
	SystemPrompt string
	Options      map[string]interface{}
}

// Message 定义对话条目
type Message struct {
	Role         string
	Content      string
	MultiContent []ContentPart
	Name         string
	ToolCallID   string
	ToolCalls    []ToolCall
}

// ContentPart 定义多模态内容块
type ContentPart struct {
	Type     string // "text", "image_url", "image_base64"
	Text     string
	ImageURL string
	MimeType string
	Data     string // Base64 encoded data
}

// ToolDefinition 定义 LLM 可以调用的工具结构
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
	UserAgent         string
	DisableHTTP2      bool
	SupportsTools     bool
	SupportsStreaming bool
	SupportsVision    bool
}

// ChatResponse 是统一的标准响应
type ChatResponse struct {
	Content      string
	ToolCalls    []ToolCall
	Usage        UsageStats
	FinishReason string
}

type ToolCall struct {
	ID       string
	Function string
	Args     string
}

type UsageStats struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// StreamEvent 定义流式响应事件
type StreamEvent struct {
	Content         string
	ToolCalls       []ToolCall
	Usage           *UsageStats
	FinishReason    string
	Err             error
	EventType       string
	ToolName        string
	ToolCallID      string
	ToolArgs        string
	ToolResult      string
	DurationSeconds float64
	Payload         map[string]interface{}
}

// Provider 定义 LLM 调用接口
type Provider interface {
	ChatCompletion(ctx context.Context, messages []Message) (string, error)
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}
