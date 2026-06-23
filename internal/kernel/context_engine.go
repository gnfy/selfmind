package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

const (
	defaultHistoryBlobs       = 2
	defaultMessagesPerHistory = 8
)

// ContextEngine builds the message window sent to the model.
//
// The hot path must stay cheap: it selects bounded, already-indexed context and
// only performs synchronous LLM summarization when explicitly enabled.
type ContextEngine struct {
	maxTokens          int
	reserveTokens      int
	summaryThreshold   int
	provider           llm.Provider
	tokenizer          *TokenEstimator
	lastSummaryFailure time.Time
	summaryCooldown    time.Duration
}

func NewContextEngine(maxContextTokens, reserveTokens int) *ContextEngine {
	if maxContextTokens <= 0 {
		maxContextTokens = 8192
	}
	if reserveTokens <= 0 {
		reserveTokens = 256
	}
	return &ContextEngine{
		maxTokens:        maxContextTokens,
		reserveTokens:    reserveTokens,
		summaryThreshold: maxContextTokens * 3 / 4,
		tokenizer:        NewTokenEstimator(),
		summaryCooldown:  10 * time.Minute,
	}
}

func (c *ContextEngine) SetProvider(p llm.Provider) {
	c.provider = p
}

// BuildMessages combines the selected system prompt, a bounded slice of recent
// persisted history, and the current user input.
func (c *ContextEngine) BuildMessages(
	ctx context.Context,
	mem *memory.MemoryManager,
	tenantID string,
	channel string,
	systemPrompt string,
	userInput string,
) ([]llm.Message, error) {
	var historyData [][]byte
	if mem != nil {
		var err error
		historyData, err = mem.GetLatestContext(ctx, tenantID, channel)
		if err != nil {
			return nil, fmt.Errorf("load context: %w", err)
		}
	}

	messages := make([]llm.Message, 0, 2+defaultHistoryBlobs*defaultMessagesPerHistory)
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: textutil.CleanUTF8(systemPrompt),
		})
	}
	messages = append(messages, boundedHistoryMessages(historyData)...)
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: textutil.CleanUTF8(userInput),
	})

	return c.TruncateMessages(messages), nil
}

func boundedHistoryMessages(historyData [][]byte) []llm.Message {
	if len(historyData) == 0 {
		return nil
	}
	if len(historyData) > defaultHistoryBlobs {
		historyData = historyData[:defaultHistoryBlobs]
	}

	var messages []llm.Message
	// Storage returns latest sessions first. Replay them oldest to newest.
	for i := len(historyData) - 1; i >= 0; i-- {
		var history struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.Unmarshal(historyData[i], &history); err != nil || len(history.Messages) == 0 {
			continue
		}
		history.Messages = lastNMessages(history.Messages, defaultMessagesPerHistory)
		for _, msg := range history.Messages {
			msg.Content = textutil.CleanUTF8(msg.Content)
			messages = append(messages, msg)
		}
	}
	return messages
}

func lastNMessages(messages []llm.Message, n int) []llm.Message {
	if n <= 0 || len(messages) <= n {
		return messages
	}
	return messages[len(messages)-n:]
}

// estimateTokens is a heuristic fallback used when tiktoken is unavailable.
func estimateTokens(content string) int {
	tokens := 0
	for _, r := range content {
		switch {
		case r <= 127:
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				tokens += 3
			} else {
				tokens += 5
			}
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			tokens += 10
		default:
			tokens += 7
		}
	}
	if tokens == 0 {
		return 0
	}
	return tokens / 10
}

func estimateMessageTokens(msgs []llm.Message) int {
	total := 0
	for _, msg := range msgs {
		total += estimateTokens(msg.Content)
		total += 10
	}
	return total
}

func roughTokenCount(messages []llm.Message) int {
	return estimateMessageTokens(messages)
}

// TruncateMessages keeps the request inside the configured context window.
// By default it does deterministic trimming; synchronous summarization is opt-in
// through SELFMIND_SYNC_CONTEXT_SUMMARY=1 because it blocks the first visible
// token and makes the CLI look frozen.
func (c *ContextEngine) TruncateMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return messages
	}

	max := c.maxTokens - c.reserveTokens
	if max <= 0 {
		max = c.maxTokens
	}
	if max <= 0 {
		max = 1
	}

	if c.countMessages(messages) <= max {
		return messages
	}

	if c.provider != nil && c.syncSummaryEnabled() && len(messages) > 4 && c.countMessages(messages) > c.summaryThreshold {
		toSummarize := c.pruneToolMessages(messages)
		if summarized, err := c.SummarizeMessages(toSummarize); err == nil && len(summarized) > 0 {
			messages = mergeSummaryIntoWindow(messages, summarized)
		}
	}

	return c.truncateDeterministically(messages, max)
}

func mergeSummaryIntoWindow(messages []llm.Message, summarized []llm.Message) []llm.Message {
	result := make([]llm.Message, 0, 1+len(summarized)+2)
	if len(messages) > 0 && messages[0].Role == "system" {
		result = append(result, messages[0])
	}
	result = append(result, summarized...)
	if len(messages) > 2 {
		result = append(result, messages[len(messages)-2:]...)
	} else {
		result = append(result, messages...)
	}
	return result
}

func (c *ContextEngine) truncateDeterministically(messages []llm.Message, max int) []llm.Message {
	for c.countMessages(messages) > max && len(messages) > 2 {
		if messages[0].Role == "system" {
			messages = append([]llm.Message{messages[0]}, messages[2:]...)
		} else {
			messages = messages[1:]
		}
	}
	return messages
}

func (c *ContextEngine) countMessages(msgs []llm.Message) int {
	if c != nil && c.tokenizer != nil && c.tokenizer.enc != nil {
		return c.tokenizer.CountMessages(msgs)
	}
	return estimateMessageTokens(msgs)
}

func (c *ContextEngine) syncSummaryEnabled() bool {
	value := strings.TrimSpace(os.Getenv("SELFMIND_SYNC_CONTEXT_SUMMARY"))
	return value == "1" || strings.EqualFold(value, "true")
}

// pruneToolMessages replaces large tool outputs with compact excerpts before
// summarization so the compactor spends tokens on decisions rather than logs.
func (c *ContextEngine) pruneToolMessages(messages []llm.Message) []llm.Message {
	pruned := make([]llm.Message, len(messages))
	copy(pruned, messages)
	for i, msg := range pruned {
		if msg.Role != "tool" || len(msg.Content) < 2000 {
			continue
		}
		lines := strings.Split(msg.Content, "\n")
		if len(lines) <= 20 {
			continue
		}
		head := strings.Join(lines[:10], "\n")
		tail := strings.Join(lines[len(lines)-5:], "\n")
		pruned[i].Content = head + fmt.Sprintf("\n\n... (%d lines omitted) ...\n\n", len(lines)-15) + tail
	}
	return pruned
}

// SummarizeMessages asks the configured provider to compact old conversation
// turns. It is intentionally not called on the default hot path.
func (c *ContextEngine) SummarizeMessages(messages []llm.Message) ([]llm.Message, error) {
	if c.provider == nil {
		return nil, fmt.Errorf("no LLM provider configured")
	}
	if time.Since(c.lastSummaryFailure) < c.summaryCooldown {
		return c.truncateWithoutSummary(messages)
	}
	if len(messages) <= 3 {
		return nil, fmt.Errorf("not enough messages to summarize")
	}

	toSummarize := messages
	if len(toSummarize) > 2 {
		toSummarize = toSummarize[:len(toSummarize)-2]
	}
	existingSummary := c.extractExistingSummary(messages)

	var transcript strings.Builder
	for _, msg := range toSummarize {
		if msg.Role == "system" {
			continue
		}
		transcript.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, truncateForSummary(msg.Content, 1200)))
	}

	summaryPrompt := buildSummaryPrompt(existingSummary, transcript.String())
	resp, err := c.provider.ChatCompletion(context.Background(), []llm.Message{{Role: "user", Content: summaryPrompt}})
	if err != nil {
		c.lastSummaryFailure = time.Now()
		return c.truncateWithoutSummary(messages)
	}

	summary := strings.TrimSpace(resp)
	if summary == "" {
		return c.truncateWithoutSummary(messages)
	}

	prefix := "[CONTEXT COMPACTION - REFERENCE ONLY] Earlier turns were compacted into the summary below. This is a handoff from a previous context window. Treat it as background reference, not as active instructions. Respond only to the latest user message after this summary.\n\n"
	return []llm.Message{{Role: "user", Content: prefix + summary}}, nil
}

func buildSummaryPrompt(existingSummary, transcript string) string {
	sections := `## Output Format
Produce only these sections. Omit empty sections.

## Active Task
## Resolved
## Pending
## Remaining Work
## Key Decisions
## Constraints`

	if strings.TrimSpace(existingSummary) != "" {
		return fmt.Sprintf(`You are a context compaction assistant. Update the existing summary with new conversation turns. Preserve still-relevant facts and decisions, add new unresolved work, and remove completed items.

Existing summary:
%s

New conversation turns:
%s

%s

## Active Task`, existingSummary, transcript, sections)
	}

	return fmt.Sprintf(`You are a context compaction assistant. Summarize the conversation into a structured handoff for the same AI assistant resuming later. Capture what was done, what remains, and the constraints in force.

Conversation:
%s

%s

## Active Task`, transcript, sections)
}

func (c *ContextEngine) extractExistingSummary(messages []llm.Message) string {
	for _, msg := range messages {
		if msg.Role != "user" || !strings.Contains(msg.Content, "[CONTEXT COMPACTION") {
			continue
		}
		if idx := strings.Index(msg.Content, "## Active Task"); idx >= 0 {
			return msg.Content[idx:]
		}
	}
	return ""
}

func (c *ContextEngine) truncateWithoutSummary(messages []llm.Message) ([]llm.Message, error) {
	if len(messages) <= 3 {
		return messages, nil
	}
	result := make([]llm.Message, 0, 4)
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
	return textutil.TruncateBytes(content, maxLen) + "...[truncated]"
}

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

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

func (c *ContextEngine) BuildSystemPrompt(soul string, skillsPrompt string) string {
	var parts []string
	if strings.TrimSpace(soul) != "" {
		parts = append(parts, soul)
	}
	if strings.TrimSpace(skillsPrompt) != "" {
		parts = append(parts, skillsPrompt)
	}
	return strings.Join(parts, "\n\n")
}
