package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// ContextEngine 负责构建 LLM 消息、token 预算管理和上下文窗口
type ContextEngine struct {
	maxTokens        int
	reserveTokens    int // 保留给响应的 tokens
	summaryThreshold int // 当可用 tokens 低于此值时触发压缩（默认 maxTokens*3/4）
	provider         llm.Provider
}

// NewContextEngine 创建一个上下文引擎
func NewContextEngine(maxContextTokens, reserveTokens int) *ContextEngine {
	if reserveTokens <= 0 {
		reserveTokens = 256
	}
	summaryThresh := maxContextTokens * 3 / 4
	return &ContextEngine{
		maxTokens:        maxContextTokens,
		reserveTokens:    reserveTokens,
		summaryThreshold: summaryThresh,
	}
}

// SetProvider sets the LLM provider for summarization.
func (c *ContextEngine) SetProvider(p llm.Provider) {
	c.provider = p
}

// BuildMessages 将历史数据+当前输入构建为 LLM 消息列表
// channel 用于过滤特定渠道的历史记录（如 'cli'、'wechat'、'dingtalk'）
func (c *ContextEngine) BuildMessages(
	ctx context.Context,
	mem *memory.MemoryManager,
	tenantID string,
	channel string,
	systemPrompt string,
	userInput string,
) ([]llm.Message, error) {
	// 1. 加载历史上下文（按渠道隔离）
	historyData, err := mem.GetLatestContext(ctx, tenantID, channel)
	if err != nil {
		return nil, fmt.Errorf("load context: %w", err)
	}

	// 2. 构建消息列表
	var messages []llm.Message

	// system prompt
	if systemPrompt != "" {
		messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	}

	// 历史消息（如果有）
	for _, blob := range historyData {
		var history struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.Unmarshal(blob, &history); err == nil && len(history.Messages) > 0 {
			messages = append(messages, history.Messages...)
		}
	}

	// 当前用户输入
	messages = append(messages, llm.Message{Role: "user", Content: userInput})

	// 3. 如果超过 token 限制，做简单截断
	messages = c.TruncateMessages(messages)

	return messages, nil
}

// TruncateMessages 简单截断消息直到在 token 限制内
// 策略：从最旧的消息开始删除，直到满足限制
func (c *ContextEngine) TruncateMessages(messages []llm.Message) []llm.Message {
	max := c.maxTokens - c.reserveTokens

	// 粗略估算：每 4 个字符 ≈ 1 token
	roughTokenCount := func(msgs []llm.Message) int {
		total := 0
		for _, m := range msgs {
			total += len(m.Content) / 4
			total += 10 // role overhead
		}
		return total
	}

	// 如果没有 LLM provider 或消息太少，直接截断
	if c.provider == nil || len(messages) <= 3 {
		for roughTokenCount(messages) > max && len(messages) > 2 {
			messages = append([]llm.Message{messages[0]}, messages[len(messages)-1:]...)
		}
		return messages
	}

	// 当可用空间不足 summaryThreshold 且有足够消息需要压缩时，先做 summarization
	if roughTokenCount(messages) > c.summaryThreshold && len(messages) > 4 {
		summarized, err := c.SummarizeMessages(messages)
		if err == nil && len(summarized) > 0 {
			// 用摘要替换中间消息；保留 system(0) 和最后 2 条消息
			var preserved []llm.Message
			if messages[0].Role == "system" {
				preserved = append(preserved, messages[0])
			}
			// 追加摘要
			preserved = append(preserved, summarized...)
			// 保留最近两条消息（保持当前上下文）
			keep := messages[len(messages)-2:]
			preserved = append(preserved, keep...)
			messages = preserved
		}
	}

	// 再次截断确保在限制内
	for roughTokenCount(messages) > max && len(messages) > 2 {
		messages = append([]llm.Message{messages[0]}, messages[len(messages)-1:]...)
	}

	return messages
}

// roughTokenCount estimates token count for a message list (4 chars/token + role overhead).
func roughTokenCount(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		total += 10
	}
	return total
}

// SummarizeMessages asks the LLM to compress a batch of old messages into a concise summary.
// It returns a single user message containing the summary, preserving system prompt at [0].
func (c *ContextEngine) SummarizeMessages(messages []llm.Message) ([]llm.Message, error) {
	if c.provider == nil {
		return nil, fmt.Errorf("no LLM provider configured")
	}

	// Identify non-system messages to summarize (skip first=system, last 2=recent context)
	if len(messages) <= 3 {
		return nil, fmt.Errorf("not enough messages to summarize")
	}
	toSummarize := messages
	keepLast := 2
	if len(toSummarize) > keepLast {
		toSummarize = toSummarize[:len(toSummarize)-keepLast]
	}

	// Build summarization prompt
	var summaryReq struct {
		Messages string
	}
	var sb strings.Builder
	for _, m := range toSummarize {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
	}
	summaryReq.Messages = sb.String()

	summaryPrompt := fmt.Sprintf(`Please summarize the following conversation concisely, preserving key facts, decisions, and any ongoing tasks oropen questions. Output a single paragraph (2-4 sentences).

Conversation to summarize:
%s

Respond with only the summary paragraph.`, summaryReq.Messages)

	summaryMsg := []llm.Message{
		{Role: "user", Content: summaryPrompt},
	}

	resp, err := c.provider.ChatCompletion(context.Background(), summaryMsg)
	if err != nil {
		return nil, fmt.Errorf("summarization LLM call failed: %w", err)
	}

	return []llm.Message{
		{Role: "user", Content: "[Earlier conversation summarized]: " + strings.TrimSpace(resp)},
	}, nil
}

// FormatToolDefinitions 将 tools 包的工具定义转换为 LLM 格式
func (c *ContextEngine) FormatTools(toolDefs []map[string]interface{}) []llm.ToolDefinition {
	var result []llm.ToolDefinition
	for _, def := range toolDefs {
		fn, ok := def["function"].(map[string]interface{})
		if !ok {
			continue
		}
		result = append(result, llm.ToolDefinition{
			Name:        getString(fn, "name"),
			Description: getString(fn, "description"),
			Parameters:  getMap(fn, "parameters"),
		})
	}
	return result
}

// getString safe map access for string
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// getMap safe map access for map
func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

// BuildSystemPrompt 构建系统提示词，包含可用的 skills 信息
func (c *ContextEngine) BuildSystemPrompt(soul string, skillsPrompt string) string {
	var parts []string
	if soul != "" {
		parts = append(parts, soul)
	}
	if skillsPrompt != "" {
		parts = append(parts, skillsPrompt)
	}
	return strings.Join(parts, "\n\n")
}
