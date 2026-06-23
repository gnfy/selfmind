package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type AnthropicAdapter struct {
	APIKey          string
	KeyGetter       func() string
	TokenRefresher  func() string
	Model           string
	BaseURL         string
	MaxTokens       int
	Headers         map[string]string
	ReasoningEffort string
	Thinking        map[string]interface{}
	ServiceTier     string
	Quirks          ProviderQuirks
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
	Tools        []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice   interface{}        `json:"tool_choice,omitempty"`
	Thinking     interface{}        `json:"thinking,omitempty"`
	ServiceTier  string             `json:"service_tier,omitempty"`
	Stream       bool               `json:"stream,omitempty"`
}

type AnthropicTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type AnthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason string `json:"stop_reason"`
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
	anthropicReq := a.requestFromChat(req, false)

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := a.doAnthropicRequest(ctx, body, a.apiKey())
	if err != nil {
		return nil, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		if isAuthFailureStatus(resp.StatusCode, b) {
			resp.Body.Close()
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doAnthropicRequest(ctx, body, key)
				if err != nil {
					return nil, fmt.Errorf("anthropic request failed after token refresh: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return a.decodeAnthropicResponse(resp.Body)
				}
				b, _ = io.ReadAll(resp.Body)
			}
		}
		return nil, providerAPIError("anthropic", resp.StatusCode, b)
	}

	return a.decodeAnthropicResponse(resp.Body)
}

func (a *AnthropicAdapter) decodeAnthropicResponse(body io.Reader) (*ChatResponse, error) {
	var anthropicResp AnthropicResponse
	if err := json.NewDecoder(body).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	var content string
	var toolCalls []ToolCall
	for _, c := range anthropicResp.Content {
		switch c.Type {
		case "text":
			content += c.Text
		case "tool_use":
			toolCalls = append(toolCalls, ToolCall{ID: c.ID, Function: c.Name, Args: rawJSONOrObject(c.Input)})
		}
	}

	return &ChatResponse{
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: anthropicResp.StopReason,
		Usage: UsageStats{
			InputTokens:  anthropicResp.Usage.InputTokens,
			OutputTokens: anthropicResp.Usage.OutputTokens,
		},
	}, nil
}

func (a *AnthropicAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	anthropicReq := a.requestFromChat(req, true)

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := a.doAnthropicRequest(ctx, body, a.apiKey())
	if err != nil {
		return nil, fmt.Errorf("anthropic stream request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if isAuthFailureStatus(resp.StatusCode, b) {
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doAnthropicRequest(ctx, body, key)
				if err != nil {
					return nil, fmt.Errorf("anthropic stream request failed after token refresh: %w", err)
				}
				if resp.StatusCode == http.StatusOK {
					return anthropicStreamEvents(resp), nil
				}
				b, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}
		return nil, providerAPIError("anthropic", resp.StatusCode, b)
	}

	return anthropicStreamEvents(resp), nil
}

func (a *AnthropicAdapter) doAnthropicRequest(ctx context.Context, body []byte, apiKey string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	a.setHeaders(httpReq, apiKey)
	return a.httpClient().Do(httpReq)
}

func anthropicStreamEvents(resp *http.Response) <-chan StreamEvent {
	ch := make(chan StreamEvent, 256)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		reader := io.Reader(resp.Body)
		buf := make([]byte, 4096)
		var leftover []byte
		inputTokensSent := false
		lastOutputTokens := 0
		toolDeltas := make(map[int]*ToolCall)

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
					dataPart, ok := sseDataBytes(line)
					if !ok {
						continue
					}
					if bytes.Equal(dataPart, []byte("[DONE]")) {
						if calls := orderedAnthropicToolCalls(toolDeltas); len(calls) > 0 {
							ch <- StreamEvent{ToolCalls: calls}
						}
						return
					}
					var chunk struct {
						Type         string `json:"type"`
						Index        int    `json:"index"`
						ContentBlock struct {
							Type  string          `json:"type"`
							ID    string          `json:"id"`
							Name  string          `json:"name"`
							Input json.RawMessage `json:"input"`
						} `json:"content_block"`
						Delta struct {
							Type        string `json:"type"`
							Text        string `json:"text"`
							PartialJSON string `json:"partial_json"`
							StopReason  string `json:"stop_reason"`
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
					case "content_block_start":
						if chunk.ContentBlock.Type == "tool_use" {
							call := &ToolCall{
								ID:       chunk.ContentBlock.ID,
								Function: chunk.ContentBlock.Name,
								Args:     rawJSONOrNonEmptyObject(chunk.ContentBlock.Input),
							}
							toolDeltas[chunk.Index] = call
						}
					case "content_block_delta":
						if chunk.Delta.Text != "" {
							ch <- StreamEvent{Content: chunk.Delta.Text}
						}
						if chunk.Delta.PartialJSON != "" {
							call := toolDeltas[chunk.Index]
							if call == nil {
								call = &ToolCall{}
								toolDeltas[chunk.Index] = call
							}
							if strings.TrimSpace(call.Args) == "{}" || strings.TrimSpace(call.Args) == "null" {
								call.Args = ""
							}
							call.Args += chunk.Delta.PartialJSON
						}
					case "message_start":
						if !inputTokensSent && chunk.Message.Usage.InputTokens > 0 {
							inputTokensSent = true
							ch <- StreamEvent{Usage: &UsageStats{InputTokens: chunk.Message.Usage.InputTokens}}
						}
					case "message_delta":
						if chunk.Delta.StopReason != "" {
							ch <- StreamEvent{FinishReason: chunk.Delta.StopReason}
						}
						if chunk.Usage.OutputTokens > lastOutputTokens {
							delta := chunk.Usage.OutputTokens - lastOutputTokens
							lastOutputTokens = chunk.Usage.OutputTokens
							ch <- StreamEvent{Usage: &UsageStats{OutputTokens: delta}}
						}
					case "message_stop":
						if calls := orderedAnthropicToolCalls(toolDeltas); len(calls) > 0 {
							ch <- StreamEvent{ToolCalls: calls}
						}
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
	return ch
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
	return apiKeyFrom(a.APIKey, a.KeyGetter)
}

func isAnthropicOAuthToken(token string) bool {
	token = strings.TrimSpace(token)
	return strings.HasPrefix(token, "sk-ant-oat") || strings.HasPrefix(token, "cc-")
}

func (a *AnthropicAdapter) requestFromChat(req ChatRequest, stream bool) AnthropicRequest {
	anthropicReq := AnthropicRequest{
		Model:        a.Model,
		MaxTokens:    firstPositiveInt(req.MaxTokens, a.MaxTokens, 1024),
		SystemPrompt: req.SystemPrompt,
		Stream:       stream,
	}
	if len(req.Tools) > 0 {
		tools := req.Tools
		if a.usesMoonshotSchema(req) {
			tools = sanitizeMoonshotToolDefinitions(tools)
		}
		anthropicReq.Tools = anthropicTools(tools)
	}
	if req.Options != nil {
		if choice, ok := req.Options["tool_choice"]; ok {
			anthropicReq.ToolChoice = choice
		}
	}
	anthropicReq.Thinking = a.thinkingForRequest(req)
	anthropicReq.ServiceTier = a.serviceTierForRequest(req)
	systemParts := []string{}
	if strings.TrimSpace(anthropicReq.SystemPrompt) != "" {
		systemParts = append(systemParts, strings.TrimSpace(anthropicReq.SystemPrompt))
	}
	for _, m := range req.Messages {
		content := anthropicContentFromMessage(m, len(req.Tools) > 0)
		role := m.Role
		if role == "system" {
			if text := strings.TrimSpace(contentString(content)); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		if role == "tool" {
			role = "user"
			if len(req.Tools) == 0 || m.ToolCallID == "" {
				content = "TOOL_RESULT: " + contentString(content)
			}
		}
		anthropicReq.Messages = append(anthropicReq.Messages, AnthropicMessage{Role: role, Content: content})
	}
	if len(systemParts) > 0 {
		anthropicReq.SystemPrompt = strings.Join(systemParts, "\n\n")
	}
	return anthropicReq
}

func anthropicContentFromMessage(m Message, nativeTools bool) interface{} {
	if m.Role == "tool" && nativeTools && m.ToolCallID != "" {
		return []interface{}{map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": m.ToolCallID,
			"content":     contentString(openAIContentFromMessage(m)),
		}}
	}
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		blocks := []interface{}{}
		if strings.TrimSpace(m.Content) != "" {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": m.Content})
		}
		for _, call := range m.ToolCalls {
			blocks = append(blocks, map[string]interface{}{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Function,
				"input": jsonObjectOrString(call.Args),
			})
		}
		return blocks
	}
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

func (a *AnthropicAdapter) setHeaders(req *http.Request, apiKey string) {
	switch strings.ToLower(strings.TrimSpace(a.Quirks.AuthHeader)) {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case "x_api_key", "x-api-key":
		req.Header.Set("x-api-key", apiKey)
	default:
		if isMiniMaxAnthropicEndpoint(a.BaseURL) {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			req.Header.Set("x-api-key", apiKey)
		}
	}
	if req.Header.Get("x-api-key") != "" {
		if isAnthropicOAuthToken(apiKey) {
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		}
	}
	if userAgent := strings.TrimSpace(a.Quirks.UserAgent); userAgent != "" && !hasHeader(a.Headers, "User-Agent") {
		req.Header.Set("User-Agent", userAgent)
	} else if isKimiCodingEndpoint(a.BaseURL) && !hasHeader(a.Headers, "User-Agent") {
		req.Header.Set("User-Agent", "claude-code/0.1.0")
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	for key, value := range a.Headers {
		if strings.TrimSpace(key) != "" {
			req.Header.Set(key, value)
		}
	}
}

func (a *AnthropicAdapter) httpClient() *http.Client {
	if !a.Quirks.DisableHTTP2 && !isKimiCodingEndpoint(a.BaseURL) {
		return http.DefaultClient
	}
	base, _ := http.DefaultTransport.(*http.Transport)
	transport := base.Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tlsConfig := &tls.Config{NextProtos: []string{"http/1.1"}}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
		tlsConfig.NextProtos = []string{"http/1.1"}
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}
}

func (a *AnthropicAdapter) thinkingForRequest(req ChatRequest) interface{} {
	switch strings.ToLower(strings.TrimSpace(a.Quirks.ThinkingMode)) {
	case "omit", "kimi":
		return nil
	}
	if isKimiCodingEndpoint(a.BaseURL) {
		return nil
	}
	if req.Options != nil {
		if value, ok := req.Options["thinking"]; ok {
			return value
		}
	}
	if len(a.Thinking) > 0 {
		return a.Thinking
	}
	isMiniMax := a.Quirks.ThinkingMode == "minimax" || isMiniMaxAnthropicEndpoint(a.BaseURL)
	if !isMiniMax {
		return nil
	}
	if strings.EqualFold(a.Model, "MiniMax-M3") && requestRole(req) == string(RoleCodingAgent) {
		return map[string]interface{}{"type": "adaptive"}
	}
	effort := a.ReasoningEffort
	if req.Options != nil {
		if value, ok := req.Options["reasoning_effort"].(string); ok && value != "" {
			effort = value
		}
	}
	if effort != "" && strings.HasPrefix(strings.ToLower(a.Model), "minimax-m2") {
		return map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": thinkingBudget(effort),
		}
	}
	return nil
}

func (a *AnthropicAdapter) serviceTierForRequest(req ChatRequest) string {
	if req.Options != nil {
		if value, ok := req.Options["service_tier"].(string); ok && value != "" {
			return value
		}
	}
	return a.ServiceTier
}

func anthropicTools(tools []ToolDefinition) []AnthropicTool {
	out := make([]AnthropicTool, 0, len(tools))
	for _, tool := range tools {
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]interface{}{"type": "object"}
		}
		out = append(out, AnthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return out
}

func (a *AnthropicAdapter) usesMoonshotSchema(req ChatRequest) bool {
	if strings.EqualFold(strings.TrimSpace(a.Quirks.ToolSchema), "moonshot") {
		return true
	}
	return isKimiFamilyEndpoint(a.BaseURL, firstNonEmptyString(req.Model, a.Model))
}

func sanitizeMoonshotToolDefinitions(tools []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		tool.Parameters = sanitizeMoonshotParameters(tool.Parameters)
		out = append(out, tool)
	}
	return out
}

func sanitizeMoonshotParameters(parameters map[string]interface{}) map[string]interface{} {
	if parameters == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	repaired, ok := repairMoonshotSchema(parameters, true).(map[string]interface{})
	if !ok {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	if repaired["type"] != "object" {
		repaired["type"] = "object"
	}
	if _, ok := repaired["properties"]; !ok {
		repaired["properties"] = map[string]interface{}{}
	}
	return repaired
}

func repairMoonshotSchema(node interface{}, isSchema bool) interface{} {
	switch value := node.(type) {
	case []interface{}:
		out := make([]interface{}, 0, len(value))
		for _, item := range value {
			out = append(out, repairMoonshotSchema(item, true))
		}
		return out
	case map[string]interface{}:
		repaired := make(map[string]interface{}, len(value))
		for key, child := range value {
			switch {
			case moonshotSchemaMapKey(key):
				if childMap, ok := child.(map[string]interface{}); ok {
					nested := make(map[string]interface{}, len(childMap))
					for subKey, subVal := range childMap {
						nested[subKey] = repairMoonshotSchema(subVal, true)
					}
					repaired[key] = nested
				} else {
					repaired[key] = child
				}
			case moonshotSchemaListKey(key):
				if childList, ok := child.([]interface{}); ok {
					repaired[key] = repairMoonshotSchema(childList, true)
				} else {
					repaired[key] = child
				}
			case key == "items":
				if childList, ok := child.([]interface{}); ok {
					if len(childList) == 0 {
						repaired[key] = map[string]interface{}{}
					} else {
						repaired[key] = repairMoonshotSchema(childList[0], true)
					}
				} else if childMap, ok := child.(map[string]interface{}); ok {
					repaired[key] = repairMoonshotSchema(childMap, true)
				} else {
					repaired[key] = child
				}
			case moonshotSchemaNodeKey(key):
				if childMap, ok := child.(map[string]interface{}); ok {
					repaired[key] = repairMoonshotSchema(childMap, true)
				} else {
					repaired[key] = child
				}
			default:
				repaired[key] = child
			}
		}
		if !isSchema {
			return repaired
		}
		if anyOf, ok := repaired["anyOf"].([]interface{}); ok {
			delete(repaired, "type")
			nonNull := make([]interface{}, 0, len(anyOf))
			for _, branch := range anyOf {
				if branchMap, ok := branch.(map[string]interface{}); ok && branchMap["type"] == "null" {
					continue
				}
				nonNull = append(nonNull, branch)
			}
			if len(nonNull) > 0 && len(nonNull) < len(anyOf) {
				if len(nonNull) == 1 {
					merged := make(map[string]interface{}, len(repaired))
					for key, child := range repaired {
						if key != "anyOf" {
							merged[key] = child
						}
					}
					if branchMap, ok := nonNull[0].(map[string]interface{}); ok {
						for key, child := range branchMap {
							merged[key] = child
						}
					}
					repaired = merged
				} else {
					repaired["anyOf"] = nonNull
					return repaired
				}
			} else {
				return repaired
			}
		}
		delete(repaired, "nullable")
		if _, hasRef := repaired["$ref"]; !hasRef {
			repaired = fillMissingMoonshotType(repaired)
		}
		if enumValues, ok := repaired["enum"].([]interface{}); ok && moonshotScalarType(repaired["type"]) {
			cleaned := make([]interface{}, 0, len(enumValues))
			for _, item := range enumValues {
				if item == nil || item == "" {
					continue
				}
				cleaned = append(cleaned, item)
			}
			if len(cleaned) > 0 {
				repaired["enum"] = cleaned
			} else {
				delete(repaired, "enum")
			}
		}
		if ref, hasRef := repaired["$ref"]; hasRef {
			return map[string]interface{}{"$ref": ref}
		}
		return repaired
	default:
		return node
	}
}

func moonshotSchemaMapKey(key string) bool {
	switch key {
	case "properties", "patternProperties", "$defs", "definitions":
		return true
	default:
		return false
	}
}

func moonshotSchemaListKey(key string) bool {
	switch key {
	case "anyOf", "oneOf", "allOf", "prefixItems":
		return true
	default:
		return false
	}
}

func moonshotSchemaNodeKey(key string) bool {
	switch key {
	case "contains", "not", "additionalProperties", "propertyNames":
		return true
	default:
		return false
	}
}

func fillMissingMoonshotType(node map[string]interface{}) map[string]interface{} {
	if value, ok := node["type"]; ok && value != nil && value != "" {
		return node
	}
	inferred := "string"
	if _, ok := node["properties"]; ok {
		inferred = "object"
	} else if _, ok := node["required"]; ok {
		inferred = "object"
	} else if _, ok := node["additionalProperties"]; ok {
		inferred = "object"
	} else if _, ok := node["items"]; ok {
		inferred = "array"
	} else if _, ok := node["prefixItems"]; ok {
		inferred = "array"
	} else if enumValues, ok := node["enum"].([]interface{}); ok && len(enumValues) > 0 {
		switch enumValues[0].(type) {
		case bool:
			inferred = "boolean"
		case int, int8, int16, int32, int64:
			inferred = "integer"
		case uint, uint8, uint16, uint32, uint64:
			inferred = "integer"
		case float32, float64:
			inferred = "number"
		default:
			inferred = "string"
		}
	}
	node["type"] = inferred
	return node
}

func moonshotScalarType(value interface{}) bool {
	switch value {
	case "string", "integer", "number", "boolean":
		return true
	default:
		return false
	}
}

func orderedAnthropicToolCalls(calls map[int]*ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	indices := make([]int, 0, len(calls))
	for idx := range calls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	out := make([]ToolCall, 0, len(indices))
	for _, idx := range indices {
		if calls[idx] != nil && calls[idx].Function != "" {
			out = append(out, *calls[idx])
		}
	}
	return out
}

func rawJSONOrObject(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

func rawJSONOrNonEmptyObject(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return ""
	}
	return string(raw)
}

func jsonObjectOrString(value string) interface{} {
	value = strings.TrimSpace(value)
	if value == "" {
		return map[string]interface{}{}
	}
	var decoded interface{}
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded
	}
	return map[string]interface{}{"value": value}
}

func firstPositiveInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func thinkingBudget(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return 4096
	case "high":
		return 16384
	default:
		return 8192
	}
}

func requestRole(req ChatRequest) string {
	if req.Options == nil {
		return ""
	}
	if role, ok := req.Options["model_role"].(string); ok {
		return role
	}
	return ""
}

func isMiniMaxAnthropicEndpoint(baseURL string) bool {
	lower := strings.ToLower(strings.TrimSpace(baseURL))
	return strings.Contains(lower, "api.minimax.io") || strings.Contains(lower, "api.minimaxi.com")
}

func isKimiCodingEndpoint(baseURL string) bool {
	lower := strings.ToLower(strings.TrimSpace(baseURL))
	return strings.Contains(lower, "api.kimi.com/coding")
}

func isKimiFamilyEndpoint(baseURL, model string) bool {
	lowerURL := strings.ToLower(strings.TrimSpace(baseURL))
	if strings.Contains(lowerURL, "api.kimi.com") || strings.Contains(lowerURL, "moonshot.ai") || strings.Contains(lowerURL, "moonshot.cn") {
		return true
	}
	lowerModel := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(lowerModel, "/"); idx >= 0 {
		lowerModel = lowerModel[idx+1:]
	}
	for _, prefix := range []string{"kimi-", "kimi_", "moonshot-", "moonshot_", "k1.", "k1-", "k2.", "k2-", "k25"} {
		if strings.HasPrefix(lowerModel, prefix) {
			return true
		}
	}
	return false
}

func hasHeader(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}
