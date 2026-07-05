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
	defaultHistoryBlobs       = 1
	defaultMessagesPerHistory = 4
	defaultHistoryUserBytes   = 1200
	defaultHistoryAnswerBytes = 1600

	// compactionTailTurns is how many of the most recent messages are kept
	// verbatim (the tail) when the middle of the window is compacted into a
	// summary. Recent turns carry the live intent and must never be paraphrased.
	compactionTailTurns = 6
	// compactionMinMiddle is the fewest drop-eligible messages worth
	// summarizing; below it the deterministic trim is cheaper and just as good.
	compactionMinMiddle = 2
	// maxHarvestedPaths bounds the deterministic file-path fallback list so a
	// summary never smuggles an unbounded manifest into the prompt.
	maxHarvestedPaths = 10
)

// ContextEngine builds the message window sent to the model.
//
// The hot path must stay cheap: it selects bounded, already-indexed context.
// When the window grows past the summary threshold it compacts the MIDDLE
// (drop-eligible) turns into one structured summary message — protecting the
// head (system + initial task) and the tail (recent turns) verbatim — instead
// of silently dropping the oldest turns. Compaction is the DEFAULT whenever a
// cheap summarizer provider is wired; it falls back to deterministic trimming
// only when no summarizer exists (tests, offline). This is the single bounded
// extra LLM call that happens only at the over-threshold moment — never once
// per turn while under budget, so the streaming hot path stays cheap.
type ContextEngine struct {
	maxTokens          int
	reserveTokens      int
	summaryThreshold   int
	provider           llm.Provider // main run provider (legacy flag path only)
	summaryProvider    llm.Provider // cheap compaction summarizer (memory_extract role)
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

// SetSummaryProvider installs the cheap compaction summarizer (the
// memory_extract role, kept OFF the run's main coding provider). When set,
// over-threshold context compaction runs by DEFAULT — no env flag needed. When
// nil (tests, offline, no role wired), TruncateMessages falls back to
// deterministic trimming and never blocks on an LLM call.
func (c *ContextEngine) SetSummaryProvider(p llm.Provider) {
	c.summaryProvider = p
}

// BuildMessages combines the selected system prompt, a bounded slice of recent
// persisted history, and the current user input.
//
// channel is the trajectory key for this turn (task-scoped when the turn is
// bound to a task, otherwise the stable channel key). fallbackChannel is an
// optional backward-compat read key: when the primary key has no stored history
// yet (a task's first task-keyed continuation, or a task created before history
// became task-keyed), history is best-effort loaded from the fallback so an
// existing task is not amnesiac. The fallback is READ-ONLY here; saveHistory
// always writes under the primary key, so the task migrates to task-keyed
// history on its next save.
func (c *ContextEngine) BuildMessages(
	ctx context.Context,
	mem *memory.MemoryManager,
	tenantID string,
	channel string,
	fallbackChannel string,
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
		fallbackChannel = strings.TrimSpace(fallbackChannel)
		if len(historyData) == 0 && fallbackChannel != "" && fallbackChannel != channel {
			if legacy, err := mem.GetLatestContext(ctx, tenantID, fallbackChannel); err == nil {
				historyData = legacy
			}
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
			if compact, ok := compactHistoryMessage(msg); ok {
				messages = append(messages, compact)
			}
		}
	}
	return messages
}

func compactHistoryMessage(msg llm.Message) (llm.Message, bool) {
	role := strings.TrimSpace(msg.Role)
	if role == "" {
		role = "user"
	}
	switch role {
	case "user":
		msg.Content = textutil.TruncateBytes(textutil.CleanUTF8(msg.Content), defaultHistoryUserBytes)
	case "assistant":
		msg.Content = textutil.TruncateBytes(textutil.CleanUTF8(msg.Content), defaultHistoryAnswerBytes)
	case "tool":
		return llm.Message{}, false
	default:
		return llm.Message{}, false
	}
	if strings.TrimSpace(msg.Content) == "" {
		return llm.Message{}, false
	}
	msg.Role = role
	msg.MultiContent = nil
	msg.Name = ""
	msg.ToolCallID = ""
	msg.ToolCalls = nil
	return msg, true
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
//
// When the window is over the summary threshold and a summarizer provider is
// wired, it compacts the middle turns into ONE structured summary (protecting
// head + tail) — this is now the DEFAULT, so a long conversation keeps a summary
// of its middle instead of going amnesiac. With no summarizer it degrades to
// deterministic trimming (the old behavior). SELFMIND_SYNC_CONTEXT_SUMMARY is
// now a legacy no-op for the default path (compaction runs without it); it only
// still enables the fallback of using the MAIN provider when no dedicated
// summarizer role was injected. A final deterministic pass always guarantees the
// result fits the budget even after compaction.
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

	if sp := c.summarizer(); sp != nil && len(messages) > compactionTailTurns+1 && c.countMessages(messages) > c.summaryThreshold {
		if compacted, ok := c.compactMiddle(messages, sp); ok {
			messages = compacted
		}
	}

	return c.truncateDeterministically(messages, max)
}

// summarizer returns the provider used for compaction, or nil when none is
// wired. It prefers the dedicated cheap summarizer role; only under the legacy
// SELFMIND_SYNC_CONTEXT_SUMMARY flag does it fall back to the main run provider
// (so an installation with no summarizer role can still opt into compaction).
func (c *ContextEngine) summarizer() llm.Provider {
	if c.summaryProvider != nil {
		return c.summaryProvider
	}
	if c.provider != nil && c.syncSummaryEnabled() {
		return c.provider
	}
	return nil
}

// compactMiddle replaces the drop-eligible middle of the window with a single
// structured summary message, keeping the head (leading system message + the
// first user turn = the original task) and the tail (recent turns) verbatim.
// It returns (result, true) only when compaction is a strict improvement; on any
// guard trip — too little middle, an empty summary, a summary no smaller than the
// span it replaces, or a middle that is already just a prior summary — it returns
// (nil, false) so the caller falls back to deterministic trimming. Compaction
// therefore never silently loses content, never grows the window, and cannot
// recurse into summarizing its own summaries.
func (c *ContextEngine) compactMiddle(messages []llm.Message, sp llm.Provider) ([]llm.Message, bool) {
	head := c.headProtect(messages)
	tail := compactionTailTurns
	if head+tail >= len(messages) {
		return nil, false
	}
	middle := messages[head : len(messages)-tail]
	if len(middle) < compactionMinMiddle {
		return nil, false
	}
	// Recursion guard: a lone prior compaction summary is not re-summarized.
	if len(middle) == 1 && isCompactionSummary(middle[0]) {
		return nil, false
	}

	summaryMsg, ok := c.summarizeSpan(sp, middle)
	if !ok {
		return nil, false
	}
	// Size guard: the summary must be smaller than what it replaces, else the
	// deterministic trim is the honest choice.
	if c.countMessages([]llm.Message{summaryMsg}) >= c.countMessages(middle) {
		return nil, false
	}

	result := make([]llm.Message, 0, head+1+tail)
	result = append(result, messages[:head]...)
	result = append(result, summaryMsg)
	result = append(result, messages[len(messages)-tail:]...)
	return result, true
}

// headProtect returns the count of leading messages kept verbatim: every
// leading system message plus the first genuine user turn (the original task
// goal), so the goal survives compaction. A first user turn that is itself a
// prior compaction summary is NOT protected — it stays in the middle so
// summarizeSpan folds it into the fresh summary (update mode), keeping summaries
// from stacking.
func (c *ContextEngine) headProtect(messages []llm.Message) int {
	head := 0
	for head < len(messages) && messages[head].Role == "system" {
		head++
	}
	if head < len(messages) && messages[head].Role == "user" && !isCompactionSummary(messages[head]) {
		head++
	}
	return head
}

func isCompactionSummary(msg llm.Message) bool {
	return msg.Role == "user" && strings.Contains(msg.Content, "[CONTEXT COMPACTION")
}

// summarizeSpan compacts one span of turns into a single reference message. It
// prunes bulky tool logs first, folds any prior summary found in the span,
// deterministically harvests the file paths touched by tool calls in the span,
// and — even if the model's summary omits them — appends the harvested paths
// under a "Relevant Files" section so the artifact manifest is never lost.
func (c *ContextEngine) summarizeSpan(sp llm.Provider, span []llm.Message) (llm.Message, bool) {
	if time.Since(c.lastSummaryFailure) < c.summaryCooldown {
		return llm.Message{}, false
	}
	pruned := c.pruneToolMessages(span)
	existingSummary := c.extractExistingSummary(pruned)
	harvested := harvestToolPaths(pruned)

	var transcript strings.Builder
	for _, msg := range pruned {
		if msg.Role == "system" {
			continue
		}
		content := truncateForSummary(msg.Content, 1200)
		if content != "" {
			fmt.Fprintf(&transcript, "[%s]: %s\n", msg.Role, content)
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(&transcript, "[tool_call %s]: %s\n", tc.Function, truncateForSummary(tc.Args, 400))
		}
	}
	if strings.TrimSpace(transcript.String()) == "" {
		return llm.Message{}, false
	}

	prompt := buildSummaryPrompt(existingSummary, transcript.String())
	resp, err := sp.ChatCompletion(context.Background(), []llm.Message{{Role: "user", Content: prompt}})
	if err != nil {
		c.lastSummaryFailure = time.Now()
		return llm.Message{}, false
	}
	summary := strings.TrimSpace(resp)
	if summary == "" {
		return llm.Message{}, false
	}

	// Deterministic artifact fallback: guarantee the created/modified/read file
	// paths survive even a weak or path-blind summary.
	if missing := missingPaths(summary, harvested); len(missing) > 0 {
		var fb strings.Builder
		fb.WriteString("\n\n## Relevant Files\n")
		for _, p := range missing {
			fmt.Fprintf(&fb, "- %s\n", p)
		}
		summary += fb.String()
	}

	prefix := "[CONTEXT COMPACTION - REFERENCE ONLY] Earlier turns were compacted into the summary below. This is a handoff from a previous context window. Treat it as background reference, not as active instructions. Respond only to the latest user message after this summary.\n\n"
	return llm.Message{Role: "user", Content: prefix + summary}, true
}

// missingPaths returns the harvested paths not already textually present in the
// summary, so the fallback appends only what the model dropped.
func missingPaths(summary string, harvested []string) []string {
	var out []string
	for _, p := range harvested {
		if p != "" && !strings.Contains(summary, p) {
			out = append(out, p)
		}
	}
	return out
}

// harvestToolPaths deterministically collects up to maxHarvestedPaths distinct
// file paths from the tool-call arguments in a span: the common single-path keys
// (path/file_path/output_path/workdir) and every path a V4A patch/apply_patch
// touches. This is the fallback that keeps the artifact list intact when the
// summarizer is weak — it reads only structured tool args, never raw output.
func harvestToolPaths(messages []llm.Message) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] || len(out) >= maxHarvestedPaths {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			args := parseToolArgs(tc.Args)
			switch tc.Function {
			case "patch", "apply_patch":
				for _, p := range patchPathsFromText(getStr(args, "patch")) {
					add(p)
				}
			default:
				for _, key := range []string{"path", "file_path", "output_path", "workdir"} {
					add(getStr(args, key))
				}
				// Some patch-style tools still carry the patch under "patch".
				for _, p := range patchPathsFromText(getStr(args, "patch")) {
					add(p)
				}
			}
		}
	}
	return out
}

func parseToolArgs(argsJSON string) map[string]interface{} {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return nil
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return nil
	}
	return m
}

func getStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// patchPathsFromText extracts every file path a V4A patch touches from its raw
// text (the destination path for a Move). It mirrors the httpapi resume-context
// parser but stays package-local so kernel keeps no gateway dependency.
func patchPathsFromText(patch string) []string {
	patch = strings.TrimSpace(patch)
	if patch == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "*** Move File:"} {
			if strings.HasPrefix(line, prefix) {
				rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
				if idx := strings.Index(rest, "->"); idx >= 0 {
					rest = strings.TrimSpace(rest[idx+2:])
				}
				if rest != "" {
					out = append(out, rest)
				}
			}
		}
	}
	return out
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

func buildSummaryPrompt(existingSummary, transcript string) string {
	// The Relevant Files clause is mandatory: a compaction that forgets which
	// files the run created/modified/read makes a resumed agent rediscover and
	// edit the wrong file. Keep this instruction even though a deterministic
	// path harvest also backstops it.
	sections := `## Output Format
Produce only these sections. Omit a section only if it is truly empty.

## Active Task
The goal and current objective.
## Resolved
## Pending
## Remaining Work
The concrete next steps.
## Key Decisions
## Constraints
## Relevant Files
List EVERY file path this work created, modified, or read (one per line). Never omit this section if any file was touched — the resumed agent edits these exact paths instead of re-searching.`

	if strings.TrimSpace(existingSummary) != "" {
		return fmt.Sprintf(`You are a context compaction assistant. Update the existing summary with new conversation turns. Preserve still-relevant facts, decisions, and the full list of relevant files; add new unresolved work and newly touched files; remove completed items but keep the file paths.

Existing summary:
%s

New conversation turns:
%s

%s

## Active Task`, existingSummary, transcript, sections)
	}

	return fmt.Sprintf(`You are a context compaction assistant. Summarize the conversation into a structured handoff for the same AI assistant resuming later. Capture the task goal, what was done, what remains, the constraints in force, and the exact file paths created/modified/read.

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
