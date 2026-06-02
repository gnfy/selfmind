package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// AnthropicAdapter 适配 Anthropic API
type AnthropicAdapter struct {
	APIKey    string        // Initial/Default Key
	KeyGetter func() string // Dynamic Key Provider
	Model     string
	BaseURL   string
	MaxTokens int
}

// AnthropicMessage 适配 Anthropic 的 messages 格式
type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []interface{}
}

// AnthropicRequest Anthropic API 请求体
type AnthropicRequest struct {
	Model        string             `json:"model"`
	Messages     []AnthropicMessage `json:"messages"`
	MaxTokens    int                `json:"max_tokens"`
	SystemPrompt string             `json:"system,omitempty"`
}

// AnthropicResponse Anthropic API 响应体
type AnthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func NewAnthropicAdapter(apiKey string) *AnthropicAdapter {
	return &AnthropicAdapter{
		APIKey:    apiKey,
		Model:     "claude-3-5-sonnet-20241022",
		BaseURL:   "https://api.anthropic.com/v1/messages",
		MaxTokens: 1024,
	}
}

func (a *AnthropicAdapter) SetModel(model string) {
	a.Model = model
}

func (a *AnthropicAdapter) GetModel() string {
	return a.Model
}

func (a *AnthropicAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	apiKey := a.APIKey
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			apiKey = k
		}
	}

	anthropicReq := AnthropicRequest{
		Model:        a.Model,
		MaxTokens:    a.MaxTokens,
		SystemPrompt: req.SystemPrompt,
	}
	for _, m := range req.Messages {
		var content interface{}
		if len(m.MultiContent) > 0 {
			var parts []interface{}
			for _, p := range m.MultiContent {
				switch p.Type {
				case "text":
					parts = append(parts, map[string]interface{}{
						"type": "text",
						"text": p.Text,
					})
				case "image_base64":
					parts = append(parts, map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": p.MimeType,
							"data":       p.Data,
						},
					})
				}
			}
			content = parts
		} else {
			content = m.Content
		}

		role := m.Role
		if role == "tool" {
			role = "user"
			content = "TOOL_RESULT: " + contentString(content)
		}

		anthropicReq.Messages = append(anthropicReq.Messages, AnthropicMessage{
			Role:    role,
			Content: content,
		})
	}

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic API error %d: %s", resp.StatusCode, string(b))
	}

	var anthropicResp AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	var content string
	for _, c := range anthropicResp.Content {
		if c.Type == "text" {
			content += c.Text
		}
	}

	return &ChatResponse{
		Content: content,
		Usage: UsageStats{
			InputTokens:  anthropicResp.Usage.InputTokens,
			OutputTokens: anthropicResp.Usage.OutputTokens,
		},
	}, nil
}

func (a *AnthropicAdapter) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	req := ChatRequest{
		Model:    a.Model,
		Messages: messages,
	}
	resp, err := a.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// OpenAIAdapter 适配 OpenAI API
type OpenAIAdapter struct {
	APIKey    string
	KeyGetter func() string
	Model     string
	BaseURL   string
}

// OpenAIMessage OpenAI 格式的 message
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"` // string or []interface{}
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

type OpenAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCallDelta struct {
	Index    int     `json:"index"`
	ID       *string `json:"id"`
	Type     *string `json:"type"`
	Function struct {
		Name      *string `json:"name"`
		Arguments *string `json:"arguments"`
	} `json:"function"`
}

// OpenAIRequest OpenAI chat completions 请求体
type OpenAIRequest struct {
	Model             string                   `json:"model"`
	Messages          []OpenAIMessage          `json:"messages"`
	Tools             []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice        interface{}              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                    `json:"parallel_tool_calls,omitempty"`
	Stream            bool                     `json:"stream,omitempty"`
	StreamOptions     map[string]interface{}   `json:"stream_options,omitempty"`
}

// OpenAIResponse OpenAI chat completions 响应体
type OpenAIResponse struct {
	Choices []struct {
		Message struct {
			Content   *string          `json:"content"`
			ToolCalls []OpenAIToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func openAIRequestFromChat(model string, req ChatRequest, stream bool) OpenAIRequest {
	nativeTools := len(req.Tools) > 0
	openaiReq := OpenAIRequest{
		Model:    model,
		Messages: make([]OpenAIMessage, 0, len(req.Messages)),
		Stream:   stream,
	}
	for _, m := range req.Messages {
		openaiReq.Messages = append(openaiReq.Messages, openAIMessageFromLLM(m, nativeTools))
	}
	if nativeTools {
		openaiReq.Tools = openAIToolDefinitions(req.Tools)
	}
	if stream {
		openaiReq.StreamOptions = map[string]interface{}{
			"include_usage": true,
		}
	}
	if req.Options != nil {
		if choice, ok := req.Options["tool_choice"]; ok {
			openaiReq.ToolChoice = choice
		}
		if parallel, ok := req.Options["parallel_tool_calls"].(bool); ok {
			openaiReq.ParallelToolCalls = &parallel
		}
	}
	return openaiReq
}

func openAIMessageFromLLM(m Message, nativeTools bool) OpenAIMessage {
	role := m.Role
	content := openAIContentFromMessage(m)

	if role == "tool" {
		if nativeTools && m.ToolCallID != "" {
			return OpenAIMessage{
				Role:       "tool",
				Content:    contentString(content),
				Name:       m.Name,
				ToolCallID: m.ToolCallID,
			}
		}
		return OpenAIMessage{
			Role:    "user",
			Content: "TOOL_RESULT: " + contentString(content),
		}
	}

	msg := OpenAIMessage{Role: role, Content: content, Name: m.Name}
	if role == "assistant" && len(m.ToolCalls) > 0 {
		msg.ToolCalls = openAIToolCallsFromLLM(m.ToolCalls)
		if msg.Content == nil {
			msg.Content = ""
		}
	}
	return msg
}

func openAIContentFromMessage(m Message) interface{} {
	if len(m.MultiContent) == 0 {
		return m.Content
	}
	parts := make([]interface{}, 0, len(m.MultiContent))
	for _, p := range m.MultiContent {
		switch p.Type {
		case "text":
			parts = append(parts, map[string]interface{}{
				"type": "text",
				"text": p.Text,
			})
		case "image_base64":
			parts = append(parts, map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": fmt.Sprintf("data:%s;base64,%s", p.MimeType, p.Data),
				},
			})
		case "image_url":
			parts = append(parts, map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": p.ImageURL,
				},
			})
		}
	}
	return parts
}

func contentString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

func openAIToolDefinitions(tools []ToolDefinition) []map[string]interface{} {
	defs := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return defs
}

func openAIToolCallsFromLLM(calls []ToolCall) []OpenAIToolCall {
	out := make([]OpenAIToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, OpenAIToolCall{
			ID:   c.ID,
			Type: "function",
			Function: OpenAIToolFunction{
				Name:      c.Function,
				Arguments: c.Args,
			},
		})
	}
	return out
}

func llmToolCallsFromOpenAI(calls []OpenAIToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{
			ID:       c.ID,
			Function: c.Function.Name,
			Args:     c.Function.Arguments,
		})
	}
	return out
}

func chatResponseFromOpenAI(openaiResp OpenAIResponse) *ChatResponse {
	content := ""
	if openaiResp.Choices[0].Message.Content != nil {
		content = *openaiResp.Choices[0].Message.Content
	}
	return &ChatResponse{
		Content:   content,
		ToolCalls: llmToolCallsFromOpenAI(openaiResp.Choices[0].Message.ToolCalls),
		Usage: UsageStats{
			InputTokens:  openaiResp.Usage.PromptTokens,
			OutputTokens: openaiResp.Usage.CompletionTokens,
		},
	}
}

func accumulateOpenAIToolDeltas(acc map[int]*OpenAIToolCall, deltas []openAIToolCallDelta) {
	for _, delta := range deltas {
		call := acc[delta.Index]
		if call == nil {
			call = &OpenAIToolCall{Type: "function"}
			acc[delta.Index] = call
		}
		if delta.ID != nil {
			call.ID = *delta.ID
		}
		if delta.Type != nil {
			call.Type = *delta.Type
		}
		if call.Type == "" {
			call.Type = "function"
		}
		if delta.Function.Name != nil {
			call.Function.Name += *delta.Function.Name
		}
		if delta.Function.Arguments != nil {
			call.Function.Arguments += *delta.Function.Arguments
		}
	}
}

func orderedOpenAIToolCalls(acc map[int]*OpenAIToolCall) []ToolCall {
	indices := make([]int, 0, len(acc))
	for idx := range acc {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	openAICalls := make([]OpenAIToolCall, 0, len(indices))
	for _, idx := range indices {
		if acc[idx] != nil {
			openAICalls = append(openAICalls, *acc[idx])
		}
	}
	return llmToolCallsFromOpenAI(openAICalls)
}

func NewOpenAIAdapter(apiKey string) *OpenAIAdapter {
	return &OpenAIAdapter{
		APIKey:  apiKey,
		Model:   "gpt-4o",
		BaseURL: "https://api.openai.com/v1/chat/completions",
	}
}

func (a *OpenAIAdapter) SetModel(model string) {
	a.Model = model
}

func (a *OpenAIAdapter) GetModel() string {
	return a.Model
}

func (a *OpenAIAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	apiKey := a.APIKey
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			apiKey = k
		}
	}

	openaiReq := openAIRequestFromChat(a.Model, req, false)

	body, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai API error %d: %s", resp.StatusCode, string(b))
	}

	var openaiResp OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openaiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(openaiResp.Choices) == 0 {
		return nil, fmt.Errorf("no response choices")
	}

	return chatResponseFromOpenAI(openaiResp), nil
}

func (a *OpenAIAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	apiKey := a.APIKey
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			apiKey = k
		}
	}

	openaiReq := openAIRequestFromChat(a.Model, req, true)

	body, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(req.Tools) > 0 {
			legacyReq := req
			legacyReq.Tools = nil
			return a.StreamChat(ctx, legacyReq)
		}
		return nil, fmt.Errorf("openai API error %d: %s", resp.StatusCode, string(b))
	}

	ch := make(chan StreamEvent, 10)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		reader := io.Reader(resp.Body)
		buf := make([]byte, 4096)
		var leftover []byte
		toolDeltas := make(map[int]*OpenAIToolCall)

		for {
			n, err := reader.Read(buf)
			if n > 0 {
				data := append(leftover, buf[:n]...)
				lines := bytes.Split(data, []byte("\n"))

				if !bytes.HasSuffix(data, []byte("\n")) {
					leftover = lines[len(lines)-1]
					lines = lines[:len(lines)-1]
				} else {
					leftover = nil
				}

				for _, line := range lines {
					line = bytes.TrimSpace(line)
					if len(line) == 0 {
						continue
					}
					if !bytes.HasPrefix(line, []byte("data: ")) {
						continue
					}
					dataPart := line[6:]
					if string(dataPart) == "[DONE]" {
						if len(toolDeltas) > 0 {
							ch <- StreamEvent{ToolCalls: orderedOpenAIToolCalls(toolDeltas)}
						}
						return
					}

					var chunk struct {
						Choices []struct {
							Delta struct {
								Content   *string               `json:"content"`
								ToolCalls []openAIToolCallDelta `json:"tool_calls"`
							} `json:"delta"`
						} `json:"choices"`
						Usage *UsageStats `json:"usage"`
					}
					if err := json.Unmarshal(dataPart, &chunk); err != nil {
						continue
					}

					if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil && *chunk.Choices[0].Delta.Content != "" {
						ch <- StreamEvent{Content: *chunk.Choices[0].Delta.Content}
					}
					if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
						accumulateOpenAIToolDeltas(toolDeltas, chunk.Choices[0].Delta.ToolCalls)
					}
					if chunk.Usage != nil {
						ch <- StreamEvent{Usage: chunk.Usage}
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					ch <- StreamEvent{Err: err}
				}
				break
			}
		}
	}()

	return ch, nil
}

func (a *OpenAIAdapter) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	req := ChatRequest{
		Model:    a.Model,
		Messages: messages,
	}
	resp, err := a.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (a *AnthropicAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	// TODO: Implement Anthropic SSE streaming
	// For now, fallback to non-streaming behavior wrapped in a channel
	ch := make(chan StreamEvent, 1)
	go func() {
		defer close(ch)
		resp, err := a.Chat(ctx, req)
		if err != nil {
			ch <- StreamEvent{Err: err}
			return
		}
		ch <- StreamEvent{Content: resp.Content, Usage: &resp.Usage}
	}()
	return ch, nil
}

func (a *OpenRouterAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	// OpenRouter is OpenAI compatible for streaming
	openai := &OpenAIAdapter{
		APIKey:    a.APIKey,
		KeyGetter: a.KeyGetter,
		Model:     a.Model,
		BaseURL:   a.BaseURL,
	}
	return openai.StreamChat(ctx, req)
}

// GeminiAdapter 适配 Google Gemini API (OpenAI 兼容模式)
type GeminiAdapter struct {
	OpenAIAdapter
}

func NewGeminiAdapter(apiKey string) *GeminiAdapter {
	return &GeminiAdapter{
		OpenAIAdapter: OpenAIAdapter{
			APIKey:  apiKey,
			Model:   "gemini-1.5-pro",
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		},
	}
}

// MiniMaxAdapter 适配 MiniMax API
type MiniMaxAdapter struct {
	APIKey    string
	KeyGetter func() string
	Model     string
	BaseURL   string
}

func NewMiniMaxAdapter(apiKey string) *MiniMaxAdapter {
	return &MiniMaxAdapter{
		APIKey:  apiKey,
		Model:   "abab6.5s-chat",
		BaseURL: "https://api.minimax.io/v1/text/chatcompletion_v2",
	}
}

func (a *MiniMaxAdapter) SetModel(model string) {
	a.Model = model
}

func (a *MiniMaxAdapter) GetModel() string {
	return a.Model
}

func (a *MiniMaxAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// MiniMax V2 API 也是 OpenAI 兼容格式
	adapter := &OpenAIAdapter{
		APIKey:    a.APIKey,
		KeyGetter: a.KeyGetter,
		Model:     a.Model,
		BaseURL:   a.BaseURL,
	}
	return adapter.Chat(ctx, req)
}

func (a *MiniMaxAdapter) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	req := ChatRequest{
		Model:    a.Model,
		Messages: messages,
	}
	resp, err := a.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (a *MiniMaxAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	adapter := &OpenAIAdapter{
		APIKey:    a.APIKey,
		KeyGetter: a.KeyGetter,
		Model:     a.Model,
		BaseURL:   a.BaseURL,
	}
	return adapter.StreamChat(ctx, req)
}

// GenericOpenAIAdapter 这是一个通用的 OpenAI 兼容适配器
// 允许通过配置动态创建新的供应商而无需修改代码
type GenericOpenAIAdapter struct {
	OpenAIAdapter
}

func NewGenericOpenAIAdapter(name, baseURL, apiKey, model string) *GenericOpenAIAdapter {
	return &GenericOpenAIAdapter{
		OpenAIAdapter: OpenAIAdapter{
			APIKey:  apiKey,
			Model:   model,
			BaseURL: baseURL,
		},
	}
}

// ---- OpenRouter 统一适配器 ----

// OpenRouterAdapter 通过 OpenRouter 路由到多个模型
type OpenRouterAdapter struct {
	APIKey    string
	KeyGetter func() string
	Model     string
	BaseURL   string
	Client    *http.Client
}

func NewOpenRouterAdapter(apiKey string) *OpenRouterAdapter {
	return &OpenRouterAdapter{
		APIKey:  apiKey,
		Model:   "anthropic/claude-3.5-sonnet",
		BaseURL: "https://openrouter.ai/api/v1/chat/completions",
		Client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (a *OpenRouterAdapter) SetModel(model string) {
	a.Model = model
}

func (a *OpenRouterAdapter) GetModel() string {
	return a.Model
}

func (a *OpenRouterAdapter) apiKey() string {
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			return k
		}
	}
	return a.APIKey
}

func (a *OpenRouterAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	openaiReq := openAIRequestFromChat(a.Model, req, false)

	body, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey())
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("HTTP-Referer", "https://selfmind.dev")
	httpReq.Header.Set("X-Title", "SelfMind Agent")

	resp, err := a.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openrouter request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openrouter API error %d: %s", resp.StatusCode, string(b))
	}

	var openaiResp OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openaiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(openaiResp.Choices) == 0 {
		return nil, fmt.Errorf("no response choices")
	}

	return chatResponseFromOpenAI(openaiResp), nil
}

func (a *OpenRouterAdapter) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	req := ChatRequest{
		Model:    a.Model,
		Messages: messages,
	}
	resp, err := a.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
