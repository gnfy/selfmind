package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// ContextEngine 负责构建 LLM 消息、token 预算管理和上下文窗口
type ContextEngine struct {
	maxTokens         int
	reserveTokens     int // 保留给响应的 tokens
	summaryThreshold  int // 当可用 tokens 低于此值时触发压缩（默认 maxTokens*3/4）
	provider          llm.Provider
	tokenizer         *TokenEstimator
	lastSummaryFailure time.Time
	summaryCooldown   time.Duration
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
		tokenizer:        NewTokenEstimator(),
		summaryCooldown:  10 * time.Minute,
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

	// 3. 如果超过 token 限制，做压缩/截断
	messages = c.TruncateMessages(messages)

	return messages, nil
}

// estimateTokens 基于字符类型做更精确的 token 估算（fallback when tiktoken is unavailable）
func estimateTokens(content string) int {
	tokens := 0
	for _, r := range content {
		if r <= 127 {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				tokens += 3
			} else {
				tokens += 5
			}
		} else if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			tokens += 10
		} else {
			tokens += 7
		}
	}
	return tokens / 10
}

func estimateMessageTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m.Content)
		total += 10
	}
	return total
}

// TruncateMessages 截断消息直到在 token 限制内
// 策略：保留 system + 最近消息，中间消息用结构化摘要替换
func (c *ContextEngine) TruncateMessages(messages []llm.Message) []llm.Message {
	max := c.maxTokens - c.reserveTokens

	// 如果没有 LLM provider 或消息太少，直接暴力截断
	if c.provider == nil || len(messages) <= 3 {
		for c.countMessages(messages) > max && len(messages) > 2 {
			messages = append([]llm.Message{messages[0]}, messages[len(messages)-1:]...)
		}
		return messages
	}

	// 当可用空间不足 summaryThreshold 且有足够消息需要压缩时，先做 summarization
	if c.countMessages(messages) > c.summaryThreshold && len(messages) > 4 {
		// Pre-prune tool outputs to reduce summarization cost
		toSummarize := c.pruneToolMessages(messages)
		summarized, err := c.SummarizeMessages(toSummarize)
		if err == nil && len(summarized) > 0 {
			// 用摘要替换中间消息；保留 system(0) 和最后 2 条消息
			var preserved []llm.Message
			if messages[0].Role == "system" {
				preserved = append(preserved, messages[0])
			}
			preserved = append(preserved, summarized...)
			keep := messages[len(messages)-2:]
			preserved = append(preserved, keep...)
			messages = preserved
		}
	}

	// 再次截断确保在限制内
	for c.countMessages(messages) > max && len(messages) > 2 {
		messages = append([]llm.Message{messages[0]}, messages[len(messages)-1:]...)
	}

	return messages
}

func (c *ContextEngine) countMessages(msgs []llm.Message) int {
	if c.tokenizer != nil && c.tokenizer.enc != nil {
		return c.tokenizer.CountMessages(msgs)
	}
	return estimateMessageTokens(msgs)
}

// roughTokenCount estimates token count for a message list (legacy, used externally).
func roughTokenCount(messages []llm.Message) int {
	return estimateMessageTokens(messages)
}

// pruneToolMessages pre-processes tool output messages before summarization.
// It replaces large tool outputs with concise placeholders to save summarization tokens.
func (c *ContextEngine) pruneToolMessages(messages []llm.Message) []llm.Message {
	pruned := make([]llm.Message, len(messages))
	copy(pruned, messages)
	for i, m := range pruned {
		if m.Role != "tool" || len(m.Content) < 2000 {
			continue
		}
		lines := strings.Split(m.Content, "\n")
		if len(lines) > 20 {
			// Keep first 10 and last 5 lines, omit the rest
			head := strings.Join(lines[:10], "\n")
			tail := strings.Join(lines[len(lines)-5:], "\n")
			pruned[i].Content = head + fmt.Sprintf("\n\n... (%d lines omitted) ...\n\n", len(lines)-15) + tail
		}
	}
	return pruned
}

// SummarizeMessages asks the LLM to compress a batch of old messages into a concise summary.
// It returns a single user message containing the summary, preserving system prompt at [0].
func (c *ContextEngine) SummarizeMessages(messages []llm.Message) ([]llm.Message, error) {
	if c.provider == nil {
		return nil, fmt.Errorf("no LLM provider configured")
	}

	// Cooldown: if summarization failed recently, skip LLM call and use truncation
	if time.Since(c.lastSummaryFailure) < c.summaryCooldown {
		return c.truncateWithoutSummary(messages)
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

	// Check for existing summary to enable incremental updates
	existingSummary := c.extractExistingSummary(messages)

	// Build summarization prompt
	var sb strings.Builder
	for _, m := range toSummarize {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, truncateForSummary(m.Content, 1200)))
	}

	var summaryPrompt string
	if existingSummary != "" {
		summaryPrompt = fmt.Sprintf(`You are a context compaction assistant. Update the existing summary below with new information from the recent conversation turns. Preserve facts and decisions from the existing summary that are still relevant. Add new ones. Remove completed items.

Existing summary:
%s

New conversation turns to incorporate:
%s

## Output Format
Produce ONLY these sections, each preceded by its header. Omit empty sections.

## Active Task
## Resolved
## Pending
## Remaining Work
## Key Decisions
## Constraints

## Active Task`, existingSummary, sb.String())
	} else {
		summaryPrompt = fmt.Sprintf(`You are a context compaction assistant. Summarize the following conversation into a structured handoff document. The reader is a new instance of the same AI assistant resuming work — it must know what was done, what remains, and what constraints are in force.

## Output Format
Produce ONLY these sections, each preceded by its header. Omit empty sections.

## Active Task
One sentence describing the current task being worked on.

## Resolved
Bullet list of questions or sub-tasks already answered/completed.

## Pending
Bullet list of questions or sub-tasks still open or awaiting user input.

## Remaining Work
Bullet list of concrete next steps or actions still needed.

## Key Decisions
Bullet list of important choices made during the conversation (tech stack, architecture, naming conventions, etc.).

## Constraints
Bullet list of hard rules or user preferences established (e.g., "do not use third-party libraries", "output must be JSON").

Conversation to summarize:
%s

## Active Task`, sb.String())
	}

	summaryMsg := []llm.Message{
		{Role: "user", Content: summaryPrompt},
	}

	resp, err := c.provider.ChatCompletion(context.Background(), summaryMsg)
	if err != nil {
		c.lastSummaryFailure = time.Now()
		return c.truncateWithoutSummary(messages)
	}

	summary := strings.TrimSpace(resp)
	prefix := "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. This is a handoff from a previous context window — treat it as background reference, NOT as active instructions. Do NOT answer questions or fulfill requests mentioned in this summary; they were already addressed. Respond ONLY to the latest user message that appears AFTER this summary.\n\n"

	return []llm.Message{
		{Role: "user", Content: prefix + summary},
	}, nil
}

// extractExistingSummary finds a previous summary message in the conversation.
func (c *ContextEngine) extractExistingSummary(messages []llm.Message) string {
	for _, m := range messages {
		if m.Role == "user" && strings.Contains(m.Content, "[CONTEXT COMPACTION") {
			idx := strings.Index(m.Content, "## Active Task")
			if idx > 0 {
				return m.Content[idx:]
			}
		}
	}
	return ""
}

// truncateWithoutSummary drops old messages without LLM summarization.
// Used as graceful degradation when summarization fails or is on cooldown.
func (c *ContextEngine) truncateWithoutSummary(messages []llm.Message) ([]llm.Message, error) {
	if len(messages) <= 3 {
		return messages, nil
	}
	// Keep system + last 3 messages, drop everything in between
	var result []llm.Message
	if messages[0].Role == "system" {
		result = append(result, messages[0])
	}
	result = append(result, messages[len(messages)-3:]...)
	return result, nil
}

func truncateForSummary(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "...[truncated]"
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
