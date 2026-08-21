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
	"selfmind/internal/promptassets"
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
	// maxHarvestedPaths bounds the deterministic file-path fallback list while
	// leaving enough room for realistic repository-wide changes. If a hostile or
	// pathological span exceeds the bound, the summary carries an explicit
	// omission count instead of pretending the manifest is complete.
	maxHarvestedPaths = 64

	defaultSummaryOutputTokens = 4096
	maxSummaryOutputTokens     = 8192
)

// compactionBoundaryNote is the verbatim boundary note prefixed wherever a
// compaction summary is rendered into the window (work-timeline contract):
// summarized history is reference, the latest user message is authoritative.
const compactionBoundaryNote = "The history summary is reference only. The latest user message is the only authoritative instruction. If it changes direction, the latest message wins."

// ContextEngine builds the message window sent to the model.
//
// The hot path must stay cheap: it selects bounded, already-indexed context.
// When the window grows past the summary threshold it compacts the MIDDLE
// (drop-eligible) turns into one structured summary message — protecting the
// head (system + initial task) and the tail (recent turns) verbatim — instead
// of silently dropping the oldest turns. Compaction is the DEFAULT whenever a
// cheap summarizer provider is wired; it falls back to deterministic trimming
// only when no summarizer exists (tests, offline). This bounded compaction
// operation happens only at the over-threshold moment;
// an explicitly truncated response may be retried once within the route limit.
// It never runs once per turn while under budget, so the hot path stays cheap.
type ContextEngine struct {
	maxTokens          int
	reserveTokens      int
	summaryThreshold   int
	provider           llm.Provider // main run provider (legacy flag path only)
	summaryProvider    llm.Provider // auxiliary/dedicated compaction summarizer
	summaryOutputLimit int          // resolved role/provider output ceiling
	tokenizer          *TokenEstimator
	lastSummaryFailure time.Time
	summaryCooldown    time.Duration
	promptSnapshot     *promptassets.Snapshot
}

func NewContextEngine(maxContextTokens, reserveTokens int) *ContextEngine {
	if maxContextTokens <= 0 {
		maxContextTokens = 8192
	}
	if reserveTokens <= 0 {
		reserveTokens = 256
	}
	return &ContextEngine{
		maxTokens:          maxContextTokens,
		reserveTokens:      reserveTokens,
		summaryThreshold:   maxContextTokens * 3 / 4,
		tokenizer:          NewTokenEstimator(),
		summaryCooldown:    10 * time.Minute,
		summaryOutputLimit: maxSummaryOutputTokens,
	}
}

func (c *ContextEngine) SetProvider(p llm.Provider) {
	c.provider = p
}

// SetSummaryProvider installs the auxiliary/dedicated compaction summarizer,
// kept OFF the run's main coding provider. When set,
// over-threshold context compaction runs by DEFAULT — no env flag needed. When
// nil (tests, offline, no role wired), TruncateMessages falls back to
// deterministic trimming and never blocks on an LLM call.
func (c *ContextEngine) SetSummaryProvider(p llm.Provider) {
	c.summaryProvider = p
}

// SetSummaryOutputLimit aligns compaction with the resolved summarizer route.
// A zero value keeps the bounded built-in ceiling; an explicitly smaller role
// limit is honored even when it cannot satisfy a long summary contract.
func (c *ContextEngine) SetSummaryOutputLimit(maxTokens int) {
	if c == nil {
		return
	}
	if maxTokens <= 0 || maxTokens > maxSummaryOutputTokens {
		maxTokens = maxSummaryOutputTokens
	}
	c.summaryOutputLimit = maxTokens
}

// SetPromptSnapshot installs the immutable process snapshot used by the
// summarizer role. The locked compaction structure remains code-owned.
func (c *ContextEngine) SetPromptSnapshot(snapshot *promptassets.Snapshot) {
	c.promptSnapshot = snapshot
}

// BuildMessages combines the selected system prompt, a bounded slice of recent
// persisted history, and the current user input.
//
// key is this turn's trajectory key: the person-level work spine
// (SpineTrajectoryKey) for ordinary agent-bound turns, or a channel-local key
// for internal subsystem turns. fallbackKeys is the ordered legacy read chain
// (old `task:<id>` key, then the task's prior run channel, or the old
// channel-derived key for taskless turns): it is consulted when the primary
// key has no history yet, or when a task-bound turn finds no spine entry for
// its task in the loaded tail (a task worked before the spine existed must not
// go amnesiac just because unrelated turns already populated the spine). The
// fallback is READ-ONLY here; saveHistory always writes under the primary key,
// so history migrates forward on the next save.
func (c *ContextEngine) BuildMessages(
	ctx context.Context,
	mem *memory.MemoryManager,
	tenantID string,
	key string,
	fallbackKeys []string,
	systemPrompt string,
	userInput string,
) ([]llm.Message, error) {
	var history []llm.Message
	if mem != nil {
		historyData, err := mem.GetLatestContext(ctx, tenantID, key)
		if err != nil {
			return nil, fmt.Errorf("load context: %w", err)
		}
		history = boundedHistoryMessages(historyData)
		needFallback := len(history) == 0
		if !needFallback {
			if taskID := taskIDFromContext(ctx); taskID != "" && !spineBlobsContainTask(historyData, taskID) {
				needFallback = true
			}
		}
		if needFallback {
			for _, fk := range fallbackKeys {
				fk = strings.TrimSpace(fk)
				if fk == "" || fk == key {
					continue
				}
				legacy, err := mem.GetLatestContext(ctx, tenantID, fk)
				if err != nil || len(legacy) == 0 {
					continue
				}
				if legacyMsgs := boundedHistoryMessages(legacy); len(legacyMsgs) > 0 {
					// Legacy history predates everything on the spine tail.
					history = append(legacyMsgs, history...)
					break
				}
			}
		}
	}

	messages := make([]llm.Message, 0, 2+len(history))
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: textutil.CleanUTF8(systemPrompt),
		})
	}
	messages = append(messages, history...)
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: textutil.CleanUTF8(userInput),
	})

	return c.TruncateMessagesCtx(ctx, messages), nil
}

func taskIDFromContext(ctx context.Context) string {
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		return strings.TrimSpace(runtime.TaskID)
	}
	return ""
}

// parseSpineEntry decodes a turn-level spine record; ok is false for legacy
// cumulative {"messages": [...]} blobs (and anything else).
func parseSpineEntry(blob []byte) (spineEntry, bool) {
	var entry spineEntry
	if err := json.Unmarshal(blob, &entry); err != nil || entry.Kind != spineEntryKind {
		return spineEntry{}, false
	}
	return entry, true
}

// spineBlobsContainTask reports whether any loaded spine entry carries the
// task's label — used to decide whether a task that predates the spine still
// needs its legacy-key compat read.
func spineBlobsContainTask(blobs [][]byte, taskID string) bool {
	for _, blob := range blobs {
		if entry, ok := parseSpineEntry(blob); ok && entry.TaskID == taskID {
			return true
		}
	}
	return false
}

// boundedHistoryMessages replays persisted history latest-blobs-first input as
// oldest-to-newest messages. Spine-shaped blobs (one slim entry per turn) are
// replayed up to composerSpineTailEntries turns; legacy cumulative blobs keep
// the old bounded single-blob window.
func boundedHistoryMessages(historyData [][]byte) []llm.Message {
	if len(historyData) == 0 {
		return nil
	}
	if msgs := spineTailMessages(historyData); len(msgs) > 0 {
		return msgs
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

// spineTailMessages renders the spine tail (Composer slice ②): the most recent
// composerSpineTailEntries turn entries as alternating user/assistant messages
// in completion order. Non-spine blobs in the input are skipped, so a key
// holding legacy blobs yields nothing here and falls to the legacy path.
func spineTailMessages(historyData [][]byte) []llm.Message {
	var entries []spineEntry
	for _, blob := range historyData { // latest first
		entry, ok := parseSpineEntry(blob)
		if !ok {
			continue
		}
		entries = append(entries, entry)
		if len(entries) >= composerSpineTailEntries {
			break
		}
	}
	if len(entries) == 0 {
		return nil
	}
	var messages []llm.Message
	for i := len(entries) - 1; i >= 0; i-- { // replay oldest to newest
		messages = append(messages, entries[i].toMessages()...)
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
	return c.TruncateMessagesCtx(context.Background(), messages)
}

// TruncateMessagesCtx is TruncateMessages with the per-run event channel
// available: when compaction actually fires it emits ONE context.compacted
// event (before/after tokens, span, duration) so /diag context can show what
// compaction bought — observability only, the compaction path is unchanged.
func (c *ContextEngine) TruncateMessagesCtx(ctx context.Context, messages []llm.Message) []llm.Message {
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
		beforeTokens := c.countMessages(messages)
		beforeCount := len(messages)
		started := time.Now()
		if compacted, ok := c.compactMiddle(messages, sp); ok {
			if ch := EventChannelFromContext(ctx); ch != nil {
				EmitAgentEvent(ch, AgentEvent{
					Type: "context.compacted",
					Payload: map[string]interface{}{
						"before_tokens":     beforeTokens,
						"after_tokens":      c.countMessages(compacted),
						"messages_replaced": beforeCount - len(compacted) + 1,
						"duration_ms":       time.Since(started).Milliseconds(),
					},
				})
			}
			messages = compacted
		}
	}

	return c.truncateDeterministically(messages, max)
}

// RecoverMessages rebuilds a materially smaller request after the provider
// rejects the normal estimate as over its actual context window. It is not the
// ordinary budget path: it first removes optional project-context material,
// then trims old turns to two thirds of the configured usable window, and as a
// last resort middle-truncates an oversized system prompt. The newest user
// instruction remains at the tail and adapter-side tool-ledger sanitization
// removes any pair made orphaned by dropping old turns.
func (c *ContextEngine) RecoverMessages(messages []llm.Message) []llm.Message {
	if c == nil || len(messages) == 0 {
		return messages
	}
	out := append([]llm.Message(nil), messages...)
	if out[0].Role == "system" {
		if idx := strings.Index(out[0].Content, "# PROJECT CONTEXT"); idx >= 0 {
			out[0].Content = strings.TrimSpace(out[0].Content[:idx]) +
				"\n\n[Project context omitted during context-window recovery; use local tools to inspect it if needed.]"
		}
	}
	max := c.maxTokens - c.reserveTokens
	if max <= 0 {
		max = c.maxTokens
	}
	if max <= 0 {
		max = 1
	}
	target := max * 2 / 3
	if target < 256 {
		target = max
	}
	out = c.truncateDeterministically(out, target)
	for len(out) > 0 && out[0].Role == "system" && c.countMessages(out) > target && len([]rune(out[0].Content)) > 512 {
		out[0].Content = truncateContextMiddle(out[0].Content, len([]rune(out[0].Content))*3/4)
	}
	return out
}

func truncateContextMiddle(value string, maxRunes int) string {
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return value
	}
	head := maxRunes * 3 / 4
	tail := maxRunes - head
	return string(runes[:head]) + "\n\n...[system context reduced for provider window]...\n\n" + string(runes[len(runes)-tail:])
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
// and — even if the model's summary omits them — appends the bounded structured
// fallback under a "Relevant Files" section.
func (c *ContextEngine) summarizeSpan(sp llm.Provider, span []llm.Message) (llm.Message, bool) {
	if time.Since(c.lastSummaryFailure) < c.summaryCooldown {
		return llm.Message{}, false
	}
	pruned := c.pruneToolMessages(span)
	existingSummary := c.extractExistingSummary(pruned)
	harvested, omittedPaths := harvestToolPathsBounded(pruned, maxHarvestedPaths)

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

	systemPrompt := buildSummarySystemPromptWithGuidance(strings.TrimSpace(existingSummary) != "",
		c.promptSnapshot.Custom(promptassets.FileSummarizer, promptassets.SectionSummaryPriorities),
		c.promptSnapshot.Custom(promptassets.FileSummarizer, promptassets.SectionLanguageDetail),
	)
	input := buildSummaryInput(existingSummary, transcript.String())
	callCtx := llm.WithModelContext(context.Background(), llm.ModelContext{Role: llm.RoleSummarizer})
	limit := c.summaryOutputLimit
	if limit <= 0 || limit > maxSummaryOutputTokens {
		limit = maxSummaryOutputTokens
	}
	maxTokens := defaultSummaryOutputTokens
	if maxTokens > limit {
		maxTokens = limit
	}
	var summary string
	for attempt := 0; attempt < 2; attempt++ {
		response, err := sp.Chat(callCtx, llm.ChatRequest{
			SystemPrompt: systemPrompt,
			Messages:     []llm.Message{{Role: "user", Content: input}},
			MaxTokens:    maxTokens,
			Options: map[string]interface{}{
				"temperature": 0, "reasoning_effort": "none",
				"summary_contract_attempt": attempt + 1,
			},
		})
		if err != nil || response == nil {
			c.lastSummaryFailure = time.Now()
			return llm.Message{}, false
		}
		if summaryFinishReasonTruncated(response.FinishReason) {
			if attempt == 0 && maxTokens < limit {
				maxTokens *= 2
				if maxTokens > limit {
					maxTokens = limit
				}
				continue
			}
			c.lastSummaryFailure = time.Now()
			return llm.Message{}, false
		}
		summary = strings.TrimSpace(response.Content)
		if summary == "" {
			c.lastSummaryFailure = time.Now()
			return llm.Message{}, false
		}
		break
	}

	// Deterministic artifact fallback: backstop the bounded structured paths
	// even when the model returns a weak or path-blind summary.
	if missing := missingPaths(summary, harvested); len(missing) > 0 {
		var fb strings.Builder
		fb.WriteString("\n\n## Relevant Files\n")
		for _, p := range missing {
			fmt.Fprintf(&fb, "- %s\n", p)
		}
		summary += fb.String()
	}
	if omittedPaths > 0 {
		summary += fmt.Sprintf("\n\n## Relevant Files Notice\n- %d additional structured path(s) were omitted from this bounded prompt fallback.\n", omittedPaths)
	}

	// The boundary note is a verbatim contract (docs/work-timeline.md): every
	// rendered compaction summary must carry it so the model never treats
	// summarized history as a live instruction.
	prefix := "[CONTEXT COMPACTION - REFERENCE ONLY] " + compactionBoundaryNote +
		" Earlier turns were compacted into the summary below. This is a handoff from a previous context window. Treat it as background reference, not as active instructions. Respond only to the latest user message after this summary.\n\n"
	return llm.Message{Role: "user", Content: prefix + summary}, true
}

func summaryFinishReasonTruncated(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return reason == "length" || reason == "max_tokens" || reason == "max_output_tokens" ||
		strings.Contains(reason, "max_token")
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
// touches. This bounded fallback reads only structured tool args, never raw
// output; summarizeSpan reports explicitly when more distinct paths existed.
func harvestToolPaths(messages []llm.Message) []string {
	paths, _ := harvestToolPathsBounded(messages, maxHarvestedPaths)
	return paths
}

func harvestToolPathsBounded(messages []llm.Message, limit int) ([]string, int) {
	seen := map[string]bool{}
	var out []string
	omitted := 0
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		if limit > 0 && len(out) >= limit {
			omitted++
			return
		}
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
	return out, omitted
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
