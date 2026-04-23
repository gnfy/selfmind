package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AnthropicAdapter 适配 Anthropic API
type AnthropicAdapter struct {
	APIKey    string      // Initial/Default Key
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
	Model       string             `json:"model"`
	Messages    []AnthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	SystemPrompt string            `json:"system,omitempty"`
}

// AnthropicResponse Anthropic API 响应体
type AnthropicResponse struct {
	Content []struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
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
		Model:       a.Model,
		MaxTokens:   a.MaxTokens,
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

		anthropicReq.Messages = append(anthropicReq.Messages, AnthropicMessage{
			Role:    m.Role,
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
	Model   string
	BaseURL string
}

// OpenAIMessage OpenAI 格式的 message
type OpenAIMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []interface{}
}

// OpenAIRequest OpenAI chat completions 请求体
type OpenAIRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
}

// OpenAIResponse OpenAI chat completions 响应体
type OpenAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens       int `json:"total_tokens"`
	} `json:"usage"`
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

	openaiReq := OpenAIRequest{
		Model: a.Model,
	}
	for _, m := range req.Messages {
		role := m.Role
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
			content = parts
		} else {
			content = m.Content
		}

		// Fix: Map "tool" role to "user" for OpenAI compatibility if not using native tools
		if role == "tool" {
			role = "user"
			if s, ok := content.(string); ok {
				content = "TOOL_RESULT: " + s
			}
		}

		openaiReq.Messages = append(openaiReq.Messages, OpenAIMessage{
			Role:    role,
			Content: content,
		})
	}

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

	return &ChatResponse{
		Content: openaiResp.Choices[0].Message.Content,
		Usage: UsageStats{
			InputTokens:  openaiResp.Usage.PromptTokens,
			OutputTokens: openaiResp.Usage.CompletionTokens,
		},
	}, nil
}

func (a *OpenAIAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	apiKey := a.APIKey
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			apiKey = k
		}
	}

	openaiReq := map[string]interface{}{
		"model":    a.Model,
		"stream":   true,
		"messages": []map[string]interface{}{},
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	msgs := []map[string]interface{}{}
	for _, m := range req.Messages {
		role := m.Role
		content := m.Content
		if role == "tool" {
			role = "user"
			content = "TOOL_RESULT: " + content
		}
		msgs = append(msgs, map[string]interface{}{
			"role":    role,
			"content": content,
		})
	}
	openaiReq["messages"] = msgs

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
		return nil, fmt.Errorf("openai API error %d: %s", resp.StatusCode, string(b))
	}

	ch := make(chan StreamEvent, 10)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		reader := io.Reader(resp.Body)
		buf := make([]byte, 4096)
		var leftover []byte

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
						return
					}

					var chunk struct {
						Choices []struct {
							Delta struct {
								Content string `json:"content"`
							} `json:"delta"`
						} `json:"choices"`
						Usage *UsageStats `json:"usage"`
					}
					if err := json.Unmarshal(dataPart, &chunk); err != nil {
						continue
					}

					if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
						ch <- StreamEvent{Content: chunk.Choices[0].Delta.Content}
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
		APIKey:  a.APIKey,
		Model:   req.Model,
		BaseURL: a.BaseURL,
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
	APIKey  string
	KeyGetter func() string
	Model   string
	BaseURL string
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
		APIKey:  a.APIKey,
		KeyGetter: a.KeyGetter,
		Model:   a.Model,
		BaseURL: a.BaseURL,
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
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
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

func (a *OpenRouterAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	openaiReq := OpenAIRequest{
		Model: req.Model,
	}
	for _, m := range req.Messages {
		role := m.Role
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
			content = parts
		} else {
			content = m.Content
		}

		// Fix: Map "tool" role to "user" for OpenAI compatibility if not using native tools
		if role == "tool" {
			role = "user"
			if s, ok := content.(string); ok {
				content = "TOOL_RESULT: " + s
			}
		}

		openaiReq.Messages = append(openaiReq.Messages, OpenAIMessage{
			Role:    role,
			Content: content,
		})
	}

	body, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+a.APIKey)
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

	return &ChatResponse{
		Content: openaiResp.Choices[0].Message.Content,
	}, nil
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
