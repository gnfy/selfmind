package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type AnthropicAdapter struct {
	APIKey    string
	KeyGetter func() string
	Model     string
	BaseURL   string
	MaxTokens int
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type AnthropicRequest struct {
	Model        string             `json:"model"`
	Messages     []AnthropicMessage `json:"messages"`
	MaxTokens    int                `json:"max_tokens"`
	SystemPrompt string             `json:"system,omitempty"`
	Stream       bool               `json:"stream,omitempty"`
}

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
	apiKey := a.apiKey()
	anthropicReq := a.requestFromChat(req, false)

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("x-api-key", apiKey)
	if isAnthropicOAuthToken(apiKey) {
		httpReq.Header.Del("x-api-key")
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
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

func (a *AnthropicAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	apiKey := a.apiKey()
	anthropicReq := a.requestFromChat(req, true)

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("x-api-key", apiKey)
	if isAnthropicOAuthToken(apiKey) {
		httpReq.Header.Del("x-api-key")
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic stream request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic API error %d: %s", resp.StatusCode, string(b))
	}

	ch := make(chan StreamEvent, 10)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		reader := io.Reader(resp.Body)
		buf := make([]byte, 4096)
		var leftover []byte
		inputTokensSent := false
		lastOutputTokens := 0

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
					if len(line) == 0 || !bytes.HasPrefix(line, []byte("data: ")) {
						continue
					}
					dataPart := line[6:]
					var chunk struct {
						Type  string `json:"type"`
						Delta struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"delta"`
						Message struct {
							Usage struct {
								InputTokens  int `json:"input_tokens"`
								OutputTokens int `json:"output_tokens"`
							} `json:"usage"`
						} `json:"message"`
						Usage struct {
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					}
					if err := json.Unmarshal(dataPart, &chunk); err != nil {
						continue
					}
					switch chunk.Type {
					case "content_block_delta":
						if chunk.Delta.Text != "" {
							ch <- StreamEvent{Content: chunk.Delta.Text}
						}
					case "message_start":
						if !inputTokensSent && chunk.Message.Usage.InputTokens > 0 {
							inputTokensSent = true
							ch <- StreamEvent{Usage: &UsageStats{InputTokens: chunk.Message.Usage.InputTokens}}
						}
					case "message_delta":
						if chunk.Usage.OutputTokens > lastOutputTokens {
							delta := chunk.Usage.OutputTokens - lastOutputTokens
							lastOutputTokens = chunk.Usage.OutputTokens
							ch <- StreamEvent{Usage: &UsageStats{OutputTokens: delta}}
						}
					case "message_stop":
						return
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

func (a *AnthropicAdapter) apiKey() string {
	apiKey := a.APIKey
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			apiKey = k
		}
	}
	return apiKey
}

func isAnthropicOAuthToken(token string) bool {
	token = strings.TrimSpace(token)
	return strings.HasPrefix(token, "sk-ant-oat") || strings.HasPrefix(token, "cc-")
}

func (a *AnthropicAdapter) requestFromChat(req ChatRequest, stream bool) AnthropicRequest {
	anthropicReq := AnthropicRequest{
		Model:        a.Model,
		MaxTokens:    a.MaxTokens,
		SystemPrompt: req.SystemPrompt,
		Stream:       stream,
	}
	for _, m := range req.Messages {
		content := anthropicContentFromMessage(m)
		role := m.Role
		if role == "tool" {
			role = "user"
			content = "TOOL_RESULT: " + contentString(content)
		}
		anthropicReq.Messages = append(anthropicReq.Messages, AnthropicMessage{Role: role, Content: content})
	}
	return anthropicReq
}

func anthropicContentFromMessage(m Message) interface{} {
	if len(m.MultiContent) == 0 {
		return m.Content
	}
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
	return parts
}
