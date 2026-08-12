package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ResponsesAdapter talks to OpenAI/Codex Responses-compatible endpoints.
type ResponsesAdapter struct {
	APIKey          string
	KeyGetter       func() string
	TokenRefresher  func() string
	Model           string
	BaseURL         string
	Store           *bool
	RequireStream   bool
	ReasoningEffort string            // e.g. "low"/"medium"/"high"; drives the Responses reasoning field
	Headers         map[string]string // extra request headers (e.g. chatgpt-account-id)
	ExtraBody       map[string]interface{}
	ExtraQuery      map[string]interface{}
	mu              sync.RWMutex
	toolNameAlias   map[string]string
}

type responsesRequest struct {
	Model          string                   `json:"model"`
	Input          []responsesInputItem     `json:"input"`
	Tools          []map[string]interface{} `json:"tools,omitempty"`
	Stream         bool                     `json:"stream,omitempty"`
	Store          *bool                    `json:"store,omitempty"`
	Reasoning      *responsesReasoning      `json:"reasoning,omitempty"`
	PromptCacheKey string                   `json:"prompt_cache_key,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type responsesInputItem struct {
	ID        string      `json:"id,omitempty"`
	Role      string      `json:"role,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	Type      string      `json:"type,omitempty"`
	Status    string      `json:"status,omitempty"`
	CallID    string      `json:"call_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
	Output    *string     `json:"output,omitempty"`
}

type responsesResponse struct {
	OutputText        string `json:"output_text"`
	Status            string `json:"status"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		ID        string `json:"id"`
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
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

// usageStatsFromResponses maps Responses-protocol usage to UsageStats.
// OpenAI/Codex report automatic prompt-cache hits as
// usage.input_tokens_details.cached_tokens; there is no separate cache-creation
// counter, so CacheCreationInputTokens stays 0 for this protocol. Note that
// OpenAI cached input tokens are discounted, not free, so the downstream
// billed_input_tokens = input_tokens - cache_read_input_tokens is an
// approximation for this protocol.
func usageStatsFromResponses(payload responsesResponse) UsageStats {
	return UsageStats{
		InputTokens:          payload.Usage.InputTokens,
		OutputTokens:         payload.Usage.OutputTokens,
		CacheReadInputTokens: payload.Usage.InputTokensDetails.CachedTokens,
	}
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
	if a.RequireStream {
		return a.chatViaStream(ctx, req)
	}
	wire := a.requestFromChat(req, false)
	body, err := marshalWithExtraBody(wire, a.ExtraBody)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}
	resp, err := a.doRequest(ctx, body, a.apiKey())
	if err != nil {
		return nil, fmt.Errorf("responses request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		if isAuthFailureStatus(resp.StatusCode, b) {
			resp.Body.Close()
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doRequest(ctx, body, key)
				if err != nil {
					return nil, fmt.Errorf("responses request failed after token refresh: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode < 400 {
					var payload responsesResponse
					if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
						return nil, fmt.Errorf("decode responses payload: %w", err)
					}
					return a.chatResponseFromResponses(payload), nil
				}
				b, _ = io.ReadAll(resp.Body)
			}
		}
		return nil, foldRetryAfter(responsesAPIError(resp.StatusCode, b), resp.Header)
	}
	var payload responsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode responses payload: %w", err)
	}
	return a.chatResponseFromResponses(payload), nil
}

func (a *ResponsesAdapter) chatViaStream(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	ch, err := a.StreamChat(ctx, req)
	if err != nil {
		return nil, err
	}
	resp := &ChatResponse{}
	for event := range ch {
		if event.Err != nil {
			return nil, event.Err
		}
		if event.EventType == "" || event.EventType == "stream" {
			resp.Content += event.Content
		}
		if len(event.ToolCalls) > 0 {
			resp.ToolCalls = append(resp.ToolCalls, event.ToolCalls...)
		}
		if event.Usage != nil {
			resp.Usage.InputTokens += event.Usage.InputTokens
			resp.Usage.OutputTokens += event.Usage.OutputTokens
			resp.Usage.CacheReadInputTokens += event.Usage.CacheReadInputTokens
			resp.Usage.CacheCreationInputTokens += event.Usage.CacheCreationInputTokens
			resp.Usage.CacheCreationReported = resp.Usage.CacheCreationReported || event.Usage.CacheCreationReported
		}
		if event.FinishReason != "" {
			resp.FinishReason = event.FinishReason
		}
	}
	return resp, nil
}

func (a *ResponsesAdapter) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	wire := a.requestFromChat(req, true)
	body, err := marshalWithExtraBody(wire, a.ExtraBody)
	if err != nil {
		return nil, err
	}
	resp, err := a.doRequest(ctx, body, a.apiKey())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if isAuthFailureStatus(resp.StatusCode, b) {
			if key, ok := refreshedAPIKey(a.TokenRefresher); ok {
				resp, err = a.doRequest(ctx, body, key)
				if err != nil {
					return nil, err
				}
				if resp.StatusCode < 400 {
					return a.streamResponse(ctx, resp), nil
				}
				b, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}
		return nil, foldRetryAfter(responsesAPIError(resp.StatusCode, b), resp.Header)
	}
	return a.streamResponse(ctx, resp), nil
}

// streamIdleTimeout bounds how long the codex SSE stream may go without any
// new bytes before we abort. Codex Responses (store=false, stream-only) can
// silently stall mid-response — bytes simply stop arriving without an EOF — and
// because the body read blocks, a run with no outer deadline would hang
// forever. The idle timeout turns that into a normal stream error so the
// caller's retry / non-stream fallback can take over. Reasoning models pause
// while thinking, so the default is generous.
func streamIdleTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("SELFMIND_STREAM_IDLE_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if d := configuredStreamIdle.Load(); d > 0 {
		return time.Duration(d)
	}
	return DefaultStreamIdle
}

func (a *ResponsesAdapter) streamResponse(ctx context.Context, resp *http.Response) <-chan StreamEvent {
	ch := make(chan StreamEvent, 256)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		// Read lines off the body in a child goroutine so the main loop can
		// select on ctx cancellation and an idle timeout. done unblocks the
		// reader's send if we abort early so it never leaks.
		lines := make(chan string, 64)
		done := make(chan struct{})
		defer close(done)
		var scanErr error
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
			for scanner.Scan() {
				select {
				case lines <- scanner.Text():
				case <-done:
					return
				}
			}
			scanErr = scanner.Err()
			close(lines)
		}()

		var emittedText strings.Builder
		emittedToolCalls := map[string]struct{}{}
		emitText := func(text string) {
			if text == "" {
				return
			}
			emittedText.WriteString(text)
			ch <- StreamEvent{Content: text}
		}
		emitToolCall := func(call ToolCall) {
			if strings.TrimSpace(call.Function) == "" {
				return
			}
			key := call.ID
			if key == "" {
				key = call.Function + ":" + call.Args
			}
			if _, ok := emittedToolCalls[key]; ok {
				return
			}
			emittedToolCalls[key] = struct{}{}
			ch <- StreamEvent{ToolCalls: []ToolCall{call}}
		}
		// process handles one SSE line; returns true when the stream is done.
		process := func(raw string) bool {
			data, ok := sseDataString(raw)
			if !ok {
				return false
			}
			if data == "[DONE]" {
				return true
			}
			var ev map[string]interface{}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return false
			}
			switch stringValue(ev["type"]) {
			case "response.output_item.added":
				if item, ok := ev["item"].(map[string]interface{}); ok {
					if name := stringValue(item["name"]); name != "" && stringValue(item["type"]) == "function_call" {
						ch <- StreamEvent{EventType: "agent.thinking", Content: "Preparing to run " + a.originalToolName(name)}
					}
				}
			case "response.output_text.delta":
				if delta := stringValue(ev["delta"]); delta != "" {
					emitText(delta)
				}
			case "response.output_item.done":
				if item, ok := ev["item"].(map[string]interface{}); ok {
					if call := a.toolCallFromResponsesItem(item); call.Function != "" {
						emitToolCall(call)
						return false
					}
					if emittedText.Len() == 0 {
						emitText(responsesTextFromItem(item))
					}
				}
			case "response.completed":
				if response, ok := ev["response"].(map[string]interface{}); ok {
					payload := responsesResponseFromMap(response)
					if emittedText.Len() == 0 {
						emitText(responsesTextFromPayload(payload))
					}
					for _, call := range a.toolCallsFromResponses(payload) {
						emitToolCall(call)
					}
					if payload.Usage.InputTokens != 0 || payload.Usage.OutputTokens != 0 {
						usage := usageStatsFromResponses(payload)
						ch <- StreamEvent{Usage: &usage}
					}
					if reason := responsesFinishReason(payload); reason != "" {
						ch <- StreamEvent{FinishReason: reason}
					}
				}
				return true
			}
			return false
		}

		idle := streamIdleTimeout()
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				ch <- StreamEvent{Err: ctx.Err()}
				return
			case <-timer.C:
				ch <- StreamEvent{Err: fmt.Errorf("codex stream idle for %s without data; aborting", idle)}
				return
			case line, ok := <-lines:
				if !ok {
					if scanErr != nil {
						ch <- StreamEvent{Err: scanErr}
					}
					return
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
				if process(line) {
					return
				}
			}
		}
	}()
	return ch
}

func (a *ResponsesAdapter) requestFromChat(req ChatRequest, stream bool) responsesRequest {
	model := firstNonEmptyString(req.Model, a.Model)
	wire := responsesRequest{Model: model, Stream: stream, Store: a.Store, PromptCacheKey: strings.TrimSpace(req.PromptCacheKey)}
	if effort := strings.TrimSpace(a.ReasoningEffort); effort != "" {
		wire.Reasoning = &responsesReasoning{Effort: effort}
	}
	// Responses replays assistant function_call items as first-class input
	// items. A standalone historical function_call is valid replay context, but
	// a function_call_output without a matching call_id is rejected by the API.
	// Keep the adapter-local pendingToolOutputs check below instead of applying
	// the stricter chat/anthropic ledger sanitizer here.
	messages := req.Messages
	pendingToolOutputs := map[string]int{}
	for _, m := range messages {
		if m.Role == "tool" {
			callID := strings.TrimSpace(m.ToolCallID)
			if callID == "" {
				continue
			}
			if pendingToolOutputs[callID] <= 0 {
				continue
			}
			pendingToolOutputs[callID]--
			output := contentString(openAIContentFromMessage(m))
			wire.Input = append(wire.Input, responsesInputItem{
				Type:   "function_call_output",
				CallID: callID,
				Output: &output,
			})
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		content := contentString(openAIContentFromMessage(m))
		if role != "assistant" || strings.TrimSpace(content) != "" {
			wire.Input = append(wire.Input, responsesInputItem{
				Role:    role,
				Content: content,
			})
		}
		if role == "assistant" {
			for _, call := range m.ToolCalls {
				if strings.TrimSpace(call.Function) == "" {
					continue
				}
				callID := responsesCallID(call)
				wire.Input = append(wire.Input, responsesInputItem{
					ID:        responsesFunctionCallItemID(call),
					Type:      "function_call",
					Status:    "completed",
					CallID:    callID,
					Name:      responsesSafeToolName(call.Function),
					Arguments: responsesCallArguments(call.Args),
				})
				pendingToolOutputs[callID]++
			}
		}
	}
	if len(req.Tools) > 0 {
		wire.Tools = a.responsesTools(req.Tools)
	}
	return wire
}

func responsesCallID(call ToolCall) string {
	if strings.TrimSpace(call.ID) != "" {
		return strings.TrimSpace(call.ID)
	}
	return "call_" + strings.NewReplacer(" ", "_", "-", "_", ".", "_").Replace(strings.TrimSpace(call.Function))
}

func responsesFunctionCallItemID(call ToolCall) string {
	id := responsesCallID(call)
	if strings.HasPrefix(id, "fc_") {
		return id
	}
	return "fc_" + strings.TrimPrefix(id, "call_")
}

func responsesCallArguments(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "{}"
	}
	return args
}

func (a *ResponsesAdapter) doRequest(ctx context.Context, body []byte, key string) (*http.Response, error) {
	requestURL, err := urlWithExtraQuery(a.BaseURL, a.ExtraQuery)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.setHeaders(httpReq, key)
	return ProviderHTTPClient().Do(httpReq)
}

func (a *ResponsesAdapter) apiKey() string {
	return apiKeyFrom(a.APIKey, a.KeyGetter)
}

func (a *ResponsesAdapter) setHeaders(req *http.Request, key string) {
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// Provider-profile headers (including Codex compatibility headers and
	// chatgpt-account-id) are resolved before transport construction. The
	// adapter never infers wire behavior from endpoint names.
	for k, v := range a.Headers {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
}

func (a *ResponsesAdapter) responsesTools(tools []ToolDefinition) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools))
	aliases := make(map[string]string)
	for _, tool := range tools {
		name := responsesSafeToolName(tool.Name)
		if name != tool.Name {
			aliases[name] = tool.Name
		}
		out = append(out, map[string]interface{}{
			"type":        "function",
			"name":        name,
			"description": tool.Description,
			"parameters":  responsesToolParameters(tool.Parameters),
			"strict":      false,
		})
	}
	a.setToolNameAliases(aliases)
	return out
}

func (a *ResponsesAdapter) setToolNameAliases(aliases map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.toolNameAlias = aliases
}

func (a *ResponsesAdapter) originalToolName(name string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.toolNameAlias == nil {
		return name
	}
	if original, ok := a.toolNameAlias[name]; ok {
		return original
	}
	return name
}

func responsesToolParameters(params map[string]interface{}) map[string]interface{} {
	if len(params) == 0 {
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
			"required":   []interface{}{},
		}
	}
	out := normalizeToolParameters(params)
	if strings.TrimSpace(stringValue(out["type"])) == "" {
		out["type"] = "object"
	}
	if stringValue(out["type"]) == "object" {
		if _, ok := out["properties"].(map[string]interface{}); !ok {
			out["properties"] = map[string]interface{}{}
		}
		if _, ok := out["required"]; !ok {
			out["required"] = []interface{}{}
		}
	}
	return out
}

func (a *ResponsesAdapter) chatResponseFromResponses(payload responsesResponse) *ChatResponse {
	content := responsesTextFromPayload(payload)
	calls := a.toolCallsFromResponses(payload)
	return &ChatResponse{
		Content:      content,
		ToolCalls:    calls,
		Usage:        usageStatsFromResponses(payload),
		FinishReason: responsesFinishReason(payload),
	}
}

func (a *ResponsesAdapter) toolCallsFromResponses(payload responsesResponse) []ToolCall {
	var calls []ToolCall
	for _, item := range payload.Output {
		if item.Type == "function_call" && item.Name != "" {
			calls = append(calls, ToolCall{ID: item.CallID, Function: a.originalToolName(item.Name), Args: item.Arguments})
		}
	}
	return calls
}

func (a *ResponsesAdapter) toolCallFromResponsesItem(item map[string]interface{}) ToolCall {
	if stringValue(item["type"]) != "function_call" {
		return ToolCall{}
	}
	name := stringValue(item["name"])
	return ToolCall{
		ID:       stringValue(item["call_id"]),
		Function: a.originalToolName(name),
		Args:     stringValue(item["arguments"]),
	}
}

func responsesResponseFromMap(value map[string]interface{}) responsesResponse {
	var payload responsesResponse
	data, err := json.Marshal(value)
	if err != nil {
		return payload
	}
	_ = json.Unmarshal(data, &payload)
	return payload
}

func responsesTextFromPayload(payload responsesResponse) string {
	if strings.TrimSpace(payload.OutputText) != "" {
		return payload.OutputText
	}
	var content strings.Builder
	for _, item := range payload.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Text != "" {
				content.WriteString(part.Text)
			}
		}
	}
	return content.String()
}

func responsesTextFromItem(item map[string]interface{}) string {
	if stringValue(item["type"]) != "message" {
		return ""
	}
	if text := stringValue(item["text"]); text != "" {
		return text
	}
	var content strings.Builder
	parts, ok := item["content"].([]interface{})
	if !ok {
		return ""
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if text := stringValue(part["text"]); text != "" {
			content.WriteString(text)
		}
	}
	return content.String()
}

func responsesFinishReason(payload responsesResponse) string {
	if reason := strings.TrimSpace(payload.IncompleteDetails.Reason); reason != "" {
		return reason
	}
	status := strings.TrimSpace(payload.Status)
	if status == "" || status == "completed" {
		return ""
	}
	return status
}

func responsesSafeToolName(name string) string {
	original := strings.TrimSpace(name)
	if responsesToolNameValid(original) {
		return original
	}
	var b strings.Builder
	for _, r := range original {
		if isResponsesToolNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		base = "tool"
	}
	sum := sha1.Sum([]byte(original))
	return base + "_" + hex.EncodeToString(sum[:])[:8]
}

func responsesToolNameValid(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	for _, r := range name {
		if !isResponsesToolNameRune(r) {
			return false
		}
	}
	return true
}

func isResponsesToolNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' ||
		r == '_' || r == '-'
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

func responsesAPIError(status int, body []byte) error {
	text := strings.TrimSpace(string(body))
	code := ""
	message := ""
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err == nil {
		if errObj, ok := payload["error"].(map[string]interface{}); ok {
			code = stringValue(errObj["code"])
			message = stringValue(errObj["message"])
		}
		if message == "" {
			message = stringValue(payload["detail"])
		}
		if message == "" {
			message = stringValue(payload["message"])
		}
	}
	if status == http.StatusUnauthorized && (strings.EqualFold(code, "token_expired") || strings.Contains(strings.ToLower(message), "signing in again")) {
		return fmt.Errorf("Codex login expired (responses API 401). Run `codex login` or open Codex and sign in again, then retry")
	}
	if message != "" {
		return fmt.Errorf("responses API error %d: %s", status, message)
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return fmt.Errorf("responses API error %d: %s", status, text)
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

// SupportsNativeTools reports whether this transport carries ChatRequest.Tools
// natively (probe used by buildSystemPrompt to avoid double-sending schemas).
func (a *ResponsesAdapter) SupportsNativeTools() bool { return true } // Responses is a native tool-calling protocol
