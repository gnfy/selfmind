package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ResponsesAdapter talks to OpenAI/Codex Responses-compatible endpoints.
type ResponsesAdapter struct {
	APIKey    string
	KeyGetter func() string
	Model     string
	BaseURL   string
}

type responsesRequest struct {
	Model  string                   `json:"model"`
	Input  []responsesInputItem     `json:"input"`
	Tools  []map[string]interface{} `json:"tools,omitempty"`
	Stream bool                     `json:"stream,omitempty"`
}

type responsesInputItem struct {
	Role    string      `json:"role,omitempty"`
	Content interface{} `json:"content,omitempty"`
	Type    string      `json:"type,omitempty"`
	CallID  string      `json:"call_id,omitempty"`
	Output  string      `json:"output,omitempty"`
}

type responsesResponse struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func NewResponsesAdapter(apiKey, baseURL, model string) *ResponsesAdapter {
	return &ResponsesAdapter{
		APIKey:  apiKey,
		BaseURL: responsesURL(baseURL),
		Model:   model,
	}
}

func (a *ResponsesAdapter) SetModel(model string) {
	a.Model = model
}

func (a *ResponsesAdapter) GetModel() string {
	return a.Model
}

func (a *ResponsesAdapter) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	resp, err := a.Chat(ctx, ChatRequest{Messages: messages})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (a *ResponsesAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	wire := a.requestFromChat(req, false)
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.setHeaders(httpReq)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("responses request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("responses API error %d: %s", resp.StatusCode, string(b))
	}
	var payload responsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode responses payload: %w", err)
	}
	return chatResponseFromResponses(payload), nil
}

func (a *ResponsesAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	wire := a.requestFromChat(req, true)
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.setHeaders(httpReq)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("responses API error %d: %s", resp.StatusCode, string(b))
	}
	ch := make(chan StreamEvent, 10)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		var toolCalls []ToolCall
		for scanner.Scan() {
			data, ok := sseDataString(scanner.Text())
			if !ok {
				continue
			}
			if data == "[DONE]" {
				if len(toolCalls) > 0 {
					ch <- StreamEvent{ToolCalls: toolCalls}
				}
				return
			}
			var ev map[string]interface{}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			switch stringValue(ev["type"]) {
			case "response.output_text.delta":
				if delta := stringValue(ev["delta"]); delta != "" {
					ch <- StreamEvent{Content: delta}
				}
			case "response.output_item.done":
				if item, ok := ev["item"].(map[string]interface{}); ok {
					if call := toolCallFromResponsesItem(item); call.Function != "" {
						toolCalls = append(toolCalls, call)
					}
				}
			case "response.completed":
				if len(toolCalls) > 0 {
					ch <- StreamEvent{ToolCalls: toolCalls}
				}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- StreamEvent{Err: err}
		}
	}()
	return ch, nil
}

func (a *ResponsesAdapter) requestFromChat(req ChatRequest, stream bool) responsesRequest {
	model := firstNonEmptyString(req.Model, a.Model)
	wire := responsesRequest{Model: model, Stream: stream}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			wire.Input = append(wire.Input, responsesInputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: contentString(openAIContentFromMessage(m)),
			})
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		wire.Input = append(wire.Input, responsesInputItem{
			Role:    role,
			Content: contentString(openAIContentFromMessage(m)),
		})
	}
	if len(req.Tools) > 0 {
		wire.Tools = responsesTools(req.Tools)
	}
	return wire
}

func (a *ResponsesAdapter) setHeaders(req *http.Request) {
	key := a.APIKey
	if a.KeyGetter != nil {
		if k := a.KeyGetter(); k != "" {
			key = k
		}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
}

func responsesTools(tools []ToolDefinition) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]interface{}{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
			"strict":      false,
		})
	}
	return out
}

func chatResponseFromResponses(payload responsesResponse) *ChatResponse {
	content := payload.OutputText
	var calls []ToolCall
	for _, item := range payload.Output {
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Text != "" {
					content += part.Text
				}
			}
			continue
		}
		if item.Type == "function_call" && item.Name != "" {
			calls = append(calls, ToolCall{ID: item.CallID, Function: item.Name, Args: item.Arguments})
		}
	}
	return &ChatResponse{
		Content:   content,
		ToolCalls: calls,
		Usage:     UsageStats{InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens},
	}
}

func toolCallFromResponsesItem(item map[string]interface{}) ToolCall {
	if stringValue(item["type"]) != "function_call" {
		return ToolCall{}
	}
	return ToolCall{
		ID:       stringValue(item["call_id"]),
		Function: stringValue(item["name"]),
		Args:     stringValue(item["arguments"]),
	}
}

func responsesURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "https://api.openai.com/v1/responses"
	}
	if strings.HasSuffix(strings.ToLower(baseURL), "/responses") {
		return baseURL
	}
	return baseURL + "/responses"
}

func stringValue(value interface{}) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
