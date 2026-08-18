package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenAIAdapter 适配 OpenAI API
type OpenAIAdapter struct {
	APIKey          string
	KeyGetter       func() string
	TokenRefresher  func() string
	Model           string
	BaseURL         string
	Headers         map[string]string
	ExtraBody       map[string]interface{}
	ExtraQuery      map[string]interface{}
	MaxTokens       int
	ReasoningEffort string
	Thinking        map[string]interface{}
	ServiceTier     string
	Quirks          ProviderQuirks
}

// OpenAIMessage OpenAI 格式的 message
type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          interface{}      `json:"content"` // string or []interface{}
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
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
	MaxTokens         int                      `json:"max_tokens,omitempty"`
	ReasoningEffort   string                   `json:"reasoning_effort,omitempty"`
	Thinking          interface{}              `json:"thinking,omitempty"`
	ServiceTier       string                   `json:"service_tier,omitempty"`
	UserID            string                   `json:"user_id,omitempty"`
	Stream            bool                     `json:"stream,omitempty"`
	StreamOptions     map[string]interface{}   `json:"stream_options,omitempty"`
}

// OpenAIResponse OpenAI chat completions 响应体
type OpenAIResponse struct {
	Choices []struct {
		Message struct {
			Content          *string          `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []OpenAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIStreamUsage `json:"usage"`
}

func openAIRequestFromChat(model string, req ChatRequest, stream bool) OpenAIRequest {
	nativeTools := len(req.Tools) > 0
	messages := sanitizeToolMessageLedger(req.Messages)
	openaiReq := OpenAIRequest{
		Model:    model,
		Messages: make([]OpenAIMessage, 0, len(messages)),
		Stream:   stream,
	}
	for _, m := range messages {
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

	msg := OpenAIMessage{Role: role, Content: content, ReasoningContent: m.ReasoningContent, Name: m.Name}
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

func intOption(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
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
		Content:          content,
		ReasoningContent: openaiResp.Choices[0].Message.ReasoningContent,
		ToolCalls:        llmToolCallsFromOpenAI(openaiResp.Choices[0].Message.ToolCalls),
		FinishReason:     openaiResp.Choices[0].FinishReason,
		Usage:            openaiResp.Usage.usageStats(),
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

func (a *OpenAIAdapter) FingerprintRequest(ctx context.Context, req ChatRequest, stream bool) (RequestFingerprint, bool) {
	wireReq := a.requestWithQuirks(req)
	wire := openAIRequestFromChat(a.Model, wireReq, stream)
	a.applyOptions(ctx, &wire, wireReq)

	stableMessages := make([]OpenAIMessage, 0, len(wire.Messages))
	for _, message := range wire.Messages {
		if message.Role != "system" && message.Role != "developer" {
			break
		}
		stableMessages = append(stableMessages, message)
	}
	settings := struct {
		Model           string
		ReasoningEffort string
		Thinking        interface{}
		ServiceTier     string
		UserID          string
		ExtraBody       map[string]interface{}
		ExtraQuery      map[string]interface{}
		Headers         map[string]string
	}{wire.Model, wire.ReasoningEffort, wire.Thinking, wire.ServiceTier, wire.UserID, a.ExtraBody, a.ExtraQuery, a.Headers}
	prefix := struct {
		Settings interface{}
		System   []OpenAIMessage
		Tools    []map[string]interface{}
	}{settings, stableMessages, wire.Tools}
	body, err := marshalWithExtraBody(wire, a.ExtraBody)
	if err != nil {
		return RequestFingerprint{}, false
	}
	return RequestFingerprint{
		Protocol:    "openai_chat",
		PrefixHash:  requestValueHash(prefix),
		RequestHash: requestValueHash(json.RawMessage(body)),
		Blocks: map[string]string{
			"settings": requestValueHash(settings),
			"system":   requestValueHash(stableMessages),
			"tools":    requestValueHash(wire.Tools),
		},
	}, true
}

func (a *OpenAIAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	wireReq := a.requestWithQuirks(req)
	openaiReq := openAIRequestFromChat(a.Model, wireReq, false)
	a.applyOptions(ctx, &openaiReq, wireReq)

	body, err := marshalWithExtraBody(openaiReq, a.ExtraBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := a.doOpenAIRequest(ctx, body, apiKeyFrom(a.APIKey, a.KeyGetter))
	if err != nil {
		return nil, fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		if isAuthFailureStatus(resp.StatusCode, b) {
			resp.Body.Close()
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doOpenAIRequest(ctx, body, key)
				if err != nil {
					return nil, fmt.Errorf("openai request failed after token refresh: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var refreshedResp OpenAIResponse
					if err := json.NewDecoder(resp.Body).Decode(&refreshedResp); err != nil {
						return nil, fmt.Errorf("decode response: %w", err)
					}
					if len(refreshedResp.Choices) == 0 {
						return nil, fmt.Errorf("no response choices")
					}
					return chatResponseFromOpenAI(refreshedResp), nil
				}
				b, _ = io.ReadAll(resp.Body)
			}
		}
		return nil, foldRetryAfter(providerAPIError("openai", resp.StatusCode, b), resp.Header)
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
	wireReq := a.requestWithQuirks(req)
	openaiReq := openAIRequestFromChat(a.Model, wireReq, true)
	a.applyOptions(ctx, &openaiReq, wireReq)

	body, err := marshalWithExtraBody(openaiReq, a.ExtraBody)
	if err != nil {
		return nil, err
	}

	resp, err := a.doOpenAIRequest(ctx, body, apiKeyFrom(a.APIKey, a.KeyGetter))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		authFailure := isAuthFailureStatus(resp.StatusCode, b)
		if authFailure {
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doOpenAIRequest(ctx, body, key)
				if err != nil {
					return nil, err
				}
				if resp.StatusCode == http.StatusOK {
					return openAIStreamEvents(resp), nil
				}
				b, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				authFailure = isAuthFailureStatus(resp.StatusCode, b)
			}
		}
		if authFailure {
			return nil, foldRetryAfter(providerAPIError("openai", resp.StatusCode, b), resp.Header)
		}
		return nil, foldRetryAfter(providerAPIError("openai", resp.StatusCode, b), resp.Header)
	}

	return openAIStreamEvents(resp), nil
}

func (a *OpenAIAdapter) doOpenAIRequest(ctx context.Context, body []byte, apiKey string) (*http.Response, error) {
	requestURL, err := urlWithExtraQuery(a.BaseURL, a.ExtraQuery)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	a.applyHeaders(httpReq)
	return ProviderHTTPClient().Do(httpReq)
}

func openAIStreamEvents(resp *http.Response) <-chan StreamEvent {
	ch := make(chan StreamEvent, 256)
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
					dataPart, ok := sseDataBytes(line)
					if !ok {
						continue
					}
					if bytes.Equal(dataPart, []byte("[DONE]")) {
						if len(toolDeltas) > 0 {
							ch <- StreamEvent{ToolCalls: orderedOpenAIToolCalls(toolDeltas)}
						}
						return
					}

					var chunk struct {
						Choices []struct {
							Delta struct {
								Content          *string               `json:"content"`
								ReasoningContent *string               `json:"reasoning_content"`
								ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
							} `json:"delta"`
							FinishReason string `json:"finish_reason"`
						} `json:"choices"`
						Usage *openAIStreamUsage `json:"usage"`
					}
					if err := json.Unmarshal(dataPart, &chunk); err != nil {
						continue
					}

					if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != nil && *chunk.Choices[0].Delta.Content != "" {
						ch <- StreamEvent{Content: *chunk.Choices[0].Delta.Content}
					}
					if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.ReasoningContent != nil && *chunk.Choices[0].Delta.ReasoningContent != "" {
						ch <- StreamEvent{ReasoningContent: *chunk.Choices[0].Delta.ReasoningContent}
					}
					if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
						accumulateOpenAIToolDeltas(toolDeltas, chunk.Choices[0].Delta.ToolCalls)
					}
					if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
						ch <- StreamEvent{FinishReason: chunk.Choices[0].FinishReason}
					}
					if chunk.Usage != nil {
						stats := chunk.Usage.usageStats()
						if stats != (UsageStats{}) {
							ch <- StreamEvent{Usage: &stats}
						}
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

type openAIStreamUsage struct {
	PromptTokens        int  `json:"prompt_tokens"`
	CompletionTokens    int  `json:"completion_tokens"`
	InputTokens         int  `json:"input_tokens"`
	OutputTokens        int  `json:"output_tokens"`
	PromptCacheHit      *int `json:"prompt_cache_hit_tokens"`
	PromptCacheMiss     *int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u openAIStreamUsage) usageStats() UsageStats {
	in := u.InputTokens
	if in == 0 {
		in = u.PromptTokens
	}
	out := u.OutputTokens
	if out == 0 {
		out = u.CompletionTokens
	}
	stats := UsageStats{InputTokens: in, OutputTokens: out}
	if u.PromptCacheHit != nil || u.PromptCacheMiss != nil {
		stats.CacheUsageReported = true
		if u.PromptCacheHit != nil {
			stats.CacheReadInputTokens = maxUsageInt(*u.PromptCacheHit, 0)
		}
		if u.PromptCacheMiss != nil {
			stats.CacheMissInputTokens = maxUsageInt(*u.PromptCacheMiss, 0)
		}
		if stats.InputTokens == 0 {
			stats.InputTokens = stats.CacheReadInputTokens + stats.CacheMissInputTokens
		}
	} else if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		stats.CacheUsageReported = true
		stats.CacheReadInputTokens = maxUsageInt(*u.PromptTokensDetails.CachedTokens, 0)
		stats.CacheMissInputTokens = maxUsageInt(stats.InputTokens-stats.CacheReadInputTokens, 0)
	}
	if u.CompletionTokensDetails != nil {
		stats.ReasoningOutputTokens = maxUsageInt(u.CompletionTokensDetails.ReasoningTokens, 0)
	}
	return stats
}

func maxUsageInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func (a *OpenAIAdapter) applyOptions(ctx context.Context, openaiReq *OpenAIRequest, req ChatRequest) {
	if req.MaxTokens > 0 {
		openaiReq.MaxTokens = req.MaxTokens
	} else if a.MaxTokens > 0 {
		openaiReq.MaxTokens = a.MaxTokens
	}
	openaiReq.ReasoningEffort = a.ReasoningEffort
	// Assigning an empty map here would box a typed nil into the interface
	// field and defeat omitempty, sending "thinking":null to providers such as
	// Gemini's OpenAI-compatible endpoint, which rejects unknown fields.
	if len(a.Thinking) > 0 {
		openaiReq.Thinking = a.Thinking
	}
	openaiReq.ServiceTier = a.ServiceTier
	if req.Options != nil {
		if value, ok := req.Options["reasoning_effort"].(string); ok && value != "" {
			if reasoningDisabled(value) {
				openaiReq.ReasoningEffort = ""
				if strings.EqualFold(strings.TrimSpace(a.Quirks.ThinkingMode), "deepseek") {
					openaiReq.Thinking = map[string]interface{}{"type": "disabled"}
				} else {
					openaiReq.Thinking = nil
				}
			} else {
				openaiReq.ReasoningEffort = value
			}
		}
		if value, ok := req.Options["thinking"]; ok {
			openaiReq.Thinking = value
		}
		if value, ok := req.Options["service_tier"].(string); ok && value != "" {
			openaiReq.ServiceTier = value
		}
		if value, ok := intOption(req.Options["max_tokens"]); ok && value > 0 {
			openaiReq.MaxTokens = value
		}
	}
	a.applyProviderRequestOptions(ctx, openaiReq)
}

func (a *OpenAIAdapter) applyProviderRequestOptions(ctx context.Context, openaiReq *OpenAIRequest) {
	if openaiReq == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(a.Quirks.ThinkingMode), "deepseek") {
		if !thinkingOptionConfigured(openaiReq.Thinking) {
			openaiReq.Thinking = map[string]interface{}{"type": "enabled"}
		}
		switch strings.ToLower(strings.TrimSpace(openaiReq.ReasoningEffort)) {
		case "xhigh", "max":
			openaiReq.ReasoningEffort = "max"
		case "low", "medium", "high":
			openaiReq.ReasoningEffort = "high"
		}
		// DeepSeek thinking mode does not accept tool_choice.
		openaiReq.ToolChoice = nil
	}
	identityField := strings.ToLower(strings.TrimSpace(a.Quirks.UserIdentityField))
	if identityField == "auto" || identityField == "user_id" {
		openaiReq.UserID = StableProviderUserID(ctx)
	}
}

func thinkingOptionConfigured(value interface{}) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		return len(typed) > 0
	default:
		return true
	}
}

func (a *OpenAIAdapter) requestWithQuirks(req ChatRequest) ChatRequest {
	if len(req.Tools) == 0 {
		return req
	}
	req.Tools = normalizeToolDefinitions(req.Tools)
	if strings.EqualFold(strings.TrimSpace(a.Quirks.ToolSchema), "moonshot") {
		req.Tools = sanitizeMoonshotToolDefinitions(req.Tools)
	}
	return req
}

func (a *OpenAIAdapter) applyHeaders(req *http.Request) {
	for key, value := range a.Headers {
		if key != "" {
			req.Header.Set(key, value)
		}
	}
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

func (a *OpenRouterAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	// OpenRouter is OpenAI compatible for streaming
	openai := &OpenAIAdapter{
		APIKey:          a.APIKey,
		KeyGetter:       a.KeyGetter,
		TokenRefresher:  a.TokenRefresher,
		Model:           a.Model,
		BaseURL:         a.BaseURL,
		Headers:         openRouterStreamHeaders(a.Headers),
		ExtraBody:       a.ExtraBody,
		ExtraQuery:      a.ExtraQuery,
		MaxTokens:       a.MaxTokens,
		ReasoningEffort: a.ReasoningEffort,
		Thinking:        a.Thinking,
		ServiceTier:     a.ServiceTier,
		Quirks:          a.Quirks,
	}
	return openai.StreamChat(ctx, req)
}

// GeminiAdapter 适配 Google Gemini API (OpenAI 兼容模式)
func (a *OpenAIAdapter) SupportsNativeTools() bool { return a.Quirks.SupportsTools }

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
	APIKey         string
	KeyGetter      func() string
	TokenRefresher func() string
	Model          string
	BaseURL        string
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
		APIKey:         a.APIKey,
		KeyGetter:      a.KeyGetter,
		TokenRefresher: a.TokenRefresher,
		Model:          a.Model,
		BaseURL:        a.BaseURL,
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
		APIKey:         a.APIKey,
		KeyGetter:      a.KeyGetter,
		TokenRefresher: a.TokenRefresher,
		Model:          a.Model,
		BaseURL:        a.BaseURL,
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

// OpenRouter app attribution. The resolver-driven path carries these as
// built-in profile headers (modelruntime), which reach every protocol; these
// constants keep the legacy direct-construction path (app.buildProvider with a
// bare API key) attributed as well.
const (
	openRouterRefererHeader = "HTTP-Referer"
	openRouterTitleHeader   = "X-Title"
	openRouterReferer       = "https://github.com/gnfy/selfmind"
	openRouterTitle         = "SelfMind Agent"
)

// openRouterStreamHeaders keeps attribution on the streaming path. StreamChat
// delegates to a plain OpenAI adapter that knows nothing about OpenRouter's
// request builder, so the headers have to travel as ordinary headers or they
// are simply lost — which is what happened to every streamed call. Configured
// headers still win, including a differently cased spelling of the same name.
func openRouterStreamHeaders(headers map[string]string) map[string]string {
	merged := make(map[string]string, len(headers)+2)
	if !hasHeader(headers, openRouterRefererHeader) {
		merged[openRouterRefererHeader] = openRouterReferer
	}
	if !hasHeader(headers, openRouterTitleHeader) {
		merged[openRouterTitleHeader] = openRouterTitle
	}
	for key, value := range headers {
		if strings.TrimSpace(key) != "" {
			merged[key] = value
		}
	}
	return merged
}

// OpenRouterAdapter 通过 OpenRouter 路由到多个模型
func (a *GenericOpenAIAdapter) SupportsNativeTools() bool { return a.Quirks.SupportsTools }

type OpenRouterAdapter struct {
	APIKey          string
	KeyGetter       func() string
	TokenRefresher  func() string
	Model           string
	BaseURL         string
	Headers         map[string]string
	ExtraBody       map[string]interface{}
	ExtraQuery      map[string]interface{}
	MaxTokens       int
	ReasoningEffort string
	Thinking        map[string]interface{}
	ServiceTier     string
	Quirks          ProviderQuirks
	Client          *http.Client
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
	options := OpenAIAdapter{
		MaxTokens:       a.MaxTokens,
		ReasoningEffort: a.ReasoningEffort,
		Thinking:        a.Thinking,
		ServiceTier:     a.ServiceTier,
		Quirks:          a.Quirks,
		ExtraBody:       a.ExtraBody,
	}
	wireReq := options.requestWithQuirks(req)
	openaiReq := openAIRequestFromChat(a.Model, wireReq, false)
	options.applyOptions(ctx, &openaiReq, wireReq)

	body, err := marshalWithExtraBody(openaiReq, a.ExtraBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := a.doOpenRouterRequest(ctx, body, a.apiKey())
	if err != nil {
		return nil, fmt.Errorf("openrouter request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		if isAuthFailureStatus(resp.StatusCode, b) {
			resp.Body.Close()
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doOpenRouterRequest(ctx, body, key)
				if err != nil {
					return nil, fmt.Errorf("openrouter request failed after token refresh: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var refreshedResp OpenAIResponse
					if err := json.NewDecoder(resp.Body).Decode(&refreshedResp); err != nil {
						return nil, fmt.Errorf("decode response: %w", err)
					}
					if len(refreshedResp.Choices) == 0 {
						return nil, fmt.Errorf("no response choices")
					}
					return chatResponseFromOpenAI(refreshedResp), nil
				}
				b, _ = io.ReadAll(resp.Body)
			}
		}
		return nil, foldRetryAfter(providerAPIError("openrouter", resp.StatusCode, b), resp.Header)
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

func (a *OpenRouterAdapter) doOpenRouterRequest(ctx context.Context, body []byte, apiKey string) (*http.Response, error) {
	requestURL, err := urlWithExtraQuery(a.BaseURL, a.ExtraQuery)
	if err != nil {
		return nil, fmt.Errorf("create request URL: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set(openRouterRefererHeader, openRouterReferer)
	httpReq.Header.Set(openRouterTitleHeader, openRouterTitle)
	for key, value := range a.Headers {
		if strings.TrimSpace(key) != "" {
			httpReq.Header.Set(key, value)
		}
	}
	return a.Client.Do(httpReq)
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

func (a *OpenRouterAdapter) SupportsNativeTools() bool { return a.Quirks.SupportsTools }
