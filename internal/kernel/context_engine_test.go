package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"selfmind/internal/kernel/llm"
)

// fakeSummarizer is a configurable-reply provider for compaction tests.
type fakeSummarizer struct {
	calls       atomic.Int32
	reply       string
	lastRequest llm.ChatRequest
}

type sequenceSummarizer struct {
	responses []llm.ChatResponse
	requests  []llm.ChatRequest
}

func (p *sequenceSummarizer) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "", nil
}

func (p *sequenceSummarizer) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.requests = append(p.requests, req)
	index := len(p.requests) - 1
	if index >= len(p.responses) {
		index = len(p.responses) - 1
	}
	response := p.responses[index]
	return &response, nil
}

func (p *sequenceSummarizer) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func (p *fakeSummarizer) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls.Add(1)
	return p.reply, nil
}

func (p *fakeSummarizer) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls.Add(1)
	p.lastRequest = req
	return &llm.ChatResponse{Content: p.reply}, nil
}

func (p *fakeSummarizer) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// compactionFixture builds an over-budget window: protected head (system + the
// original task), a large drop-eligible middle carrying file-mutating tool
// calls, and a short recent tail.
func compactionFixture() []llm.Message {
	big := strings.Repeat("context word ", 40)
	messages := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "original task: build the app"},
		{Role: "assistant", Content: big, ToolCalls: []llm.ToolCall{{Function: "write_file", Args: `{"path":"src/main.go"}`}}},
		{Role: "tool", Content: big},
		{Role: "assistant", Content: big, ToolCalls: []llm.ToolCall{{Function: "patch", Args: `{"patch":"*** Begin Patch\n*** Update File: src/util.go\n*** End Patch"}`}}},
		{Role: "tool", Content: big},
		{Role: "assistant", Content: big},
		{Role: "user", Content: big},
	}
	for i := 0; i < compactionTailTurns; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf("recent-%d", i)})
	}
	return messages
}

func TestTruncateMessagesCompactsMiddleByDefaultWithSummarizer(t *testing.T) {
	t.Setenv("SELFMIND_SYNC_CONTEXT_SUMMARY", "") // flag off: compaction must still run

	provider := &fakeSummarizer{reply: "## Active Task\nbuild the app"}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	messages := compactionFixture()
	got := engine.TruncateMessages(messages)

	if provider.calls.Load() != 1 {
		t.Fatalf("expected exactly one compaction call, got %d", provider.calls.Load())
	}
	if !strings.Contains(provider.lastRequest.SystemPrompt, "## Failed Attempts") || strings.Contains(provider.lastRequest.SystemPrompt, "original task: build the app") {
		t.Fatalf("summary contract/data separation is wrong: system=%q", provider.lastRequest.SystemPrompt)
	}
	if len(provider.lastRequest.Messages) != 1 || !strings.Contains(provider.lastRequest.Messages[0].Content, "<conversation-turns>") {
		t.Fatalf("summary input is not fenced: %#v", provider.lastRequest.Messages)
	}
	if len(got) != 2+1+compactionTailTurns {
		t.Fatalf("expected %d messages (head+summary+tail), got %d", 2+1+compactionTailTurns, len(got))
	}
	// Head preserved verbatim.
	if got[0].Role != "system" || got[0].Content != "sys" {
		t.Fatalf("system head not preserved: %+v", got[0])
	}
	if got[1].Role != "user" || got[1].Content != "original task: build the app" {
		t.Fatalf("initial task not preserved: %+v", got[1])
	}
	// One summary message carrying the harvested file paths.
	summary := got[2]
	if !strings.Contains(summary.Content, "[CONTEXT COMPACTION") {
		t.Fatalf("expected compaction summary at index 2: %+v", summary)
	}
	for _, want := range []string{"src/main.go", "src/util.go"} {
		if !strings.Contains(summary.Content, want) {
			t.Fatalf("summary missing harvested path %q:\n%s", want, summary.Content)
		}
	}
	// Tail preserved verbatim.
	tail := got[len(got)-compactionTailTurns:]
	for i, m := range tail {
		if want := fmt.Sprintf("recent-%d", i); m.Content != want {
			t.Fatalf("tail message %d = %q, want %q", i, m.Content, want)
		}
	}
	// Under budget after compaction.
	if over := engine.countMessages(got); over > 190 {
		t.Fatalf("compacted window still over budget: %d tokens", over)
	}
}

func TestSummarizerRetriesOnlyAfterExplicitTruncation(t *testing.T) {
	provider := &sequenceSummarizer{responses: []llm.ChatResponse{
		{Content: "partial handoff", FinishReason: "length"},
		{Content: "## Active Task\nbuild the app", FinishReason: "stop"},
	}}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)
	engine.SetSummaryOutputLimit(8192)

	got := engine.TruncateMessages(compactionFixture())
	if len(provider.requests) != 2 {
		t.Fatalf("summary calls = %d, want 2", len(provider.requests))
	}
	if provider.requests[0].MaxTokens != 4096 || provider.requests[1].MaxTokens != 8192 {
		t.Fatalf("summary budgets = %d then %d", provider.requests[0].MaxTokens, provider.requests[1].MaxTokens)
	}
	for _, message := range got {
		if strings.Contains(message.Content, "[CONTEXT COMPACTION") {
			return
		}
	}
	t.Fatal("successful retry did not produce a compaction summary")
}

func TestSummarizerHonorsSmallerRouteLimitAndRejectsTruncatedOutput(t *testing.T) {
	provider := &sequenceSummarizer{responses: []llm.ChatResponse{{Content: "partial handoff", FinishReason: "max_tokens"}}}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)
	engine.SetSummaryOutputLimit(2048)

	got := engine.TruncateMessages(compactionFixture())
	if len(provider.requests) != 1 || provider.requests[0].MaxTokens != 2048 {
		t.Fatalf("requests=%+v, want one 2048-token call", provider.requests)
	}
	for _, message := range got {
		if strings.Contains(message.Content, "[CONTEXT COMPACTION") {
			t.Fatal("truncated summary must not be injected")
		}
	}
}

func TestHarvestToolPathsReportsBoundedOmissions(t *testing.T) {
	messages := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{
		{Function: "write_file", Args: `{"path":"one.go"}`},
		{Function: "write_file", Args: `{"path":"two.go"}`},
		{Function: "write_file", Args: `{"path":"three.go"}`},
	}}}
	paths, omitted := harvestToolPathsBounded(messages, 2)
	if len(paths) != 2 || omitted != 1 {
		t.Fatalf("paths=%v omitted=%d", paths, omitted)
	}
}

func TestTruncateMessagesNoSummarizerFallsBackToDeterministicDrop(t *testing.T) {
	t.Setenv("SELFMIND_SYNC_CONTEXT_SUMMARY", "")

	engine := NewContextEngine(200, 10) // no summarizer wired
	messages := compactionFixture()

	got := engine.TruncateMessages(messages)
	for _, m := range got {
		if strings.Contains(m.Content, "[CONTEXT COMPACTION") {
			t.Fatalf("did not expect a compaction summary without a summarizer: %+v", m)
		}
	}
	if len(got) >= len(messages) {
		t.Fatalf("expected deterministic drop, got %d from %d", len(got), len(messages))
	}
	if got[0].Role != "system" {
		t.Fatalf("expected system prompt preserved, got %q", got[0].Role)
	}
	if over := engine.countMessages(got); over > 190 {
		t.Fatalf("deterministic trim still over budget: %d", over)
	}
}

func TestTruncateMessagesSkipsCompactionWhenSummaryNotSmaller(t *testing.T) {
	// A summary bigger than the span it replaces must be rejected in favor of the
	// deterministic trim — compaction never grows the window.
	provider := &fakeSummarizer{reply: strings.Repeat("bloated summary text ", 500)}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	got := engine.TruncateMessages(compactionFixture())
	if provider.calls.Load() != 1 {
		t.Fatalf("expected the summarizer to be consulted once, got %d", provider.calls.Load())
	}
	for _, m := range got {
		if strings.Contains(m.Content, "[CONTEXT COMPACTION") {
			t.Fatalf("oversized summary should have been rejected: %+v", m)
		}
	}
}

func TestTruncateMessagesEmptySummaryFallsBack(t *testing.T) {
	provider := &fakeSummarizer{reply: "   "} // empty after trim
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	got := engine.TruncateMessages(compactionFixture())
	for _, m := range got {
		if strings.Contains(m.Content, "[CONTEXT COMPACTION") {
			t.Fatalf("empty summary must not be injected: %+v", m)
		}
	}
	if len(got) == 0 {
		t.Fatal("expected non-empty result")
	}
}

func TestTruncateMessagesDoesNotStackSummaries(t *testing.T) {
	// A window whose earliest turn is already a compaction summary must fold it
	// into the fresh summary rather than protecting it as head and stacking a
	// second one — the recursion/growth guard.
	provider := &fakeSummarizer{reply: "## Active Task\nstill building"}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	big := strings.Repeat("context word ", 40)
	messages := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "[CONTEXT COMPACTION - REFERENCE ONLY] prior handoff\n\n## Active Task\nold"},
	}
	for i := 0; i < 6; i++ {
		messages = append(messages, llm.Message{Role: "assistant", Content: big})
	}
	for i := 0; i < compactionTailTurns; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf("recent-%d", i)})
	}

	got := engine.TruncateMessages(messages)
	count := 0
	for _, m := range got {
		if strings.Contains(m.Content, "[CONTEXT COMPACTION") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one compaction summary after folding, got %d:\n%+v", count, got)
	}
}

type contextEngineProvider struct {
	calls atomic.Int32
}

func (p *contextEngineProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls.Add(1)
	return "## Active Task\nsummary", nil
}

func (p *contextEngineProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls.Add(1)
	return &llm.ChatResponse{Content: "summary"}, nil
}

func (p *contextEngineProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func TestBoundedHistoryMessagesKeepsRecentSessionsInChronologicalOrder(t *testing.T) {
	newBlob := func(prefix string, count int) []byte {
		msgs := make([]llm.Message, 0, count)
		for i := 0; i < count; i++ {
			msgs = append(msgs, llm.Message{Role: "user", Content: prefix + string(rune('0'+i))})
		}
		blob, err := json.Marshal(struct {
			Messages []llm.Message `json:"messages"`
		}{Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}

	got := boundedHistoryMessages([][]byte{
		newBlob("new-", 10),
		newBlob("old-", 3),
		newBlob("ignored-", 3),
	})

	if len(got) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(got))
	}
	if got[0].Content != "new-6" {
		t.Fatalf("expected newest session to be trimmed to last 4 messages, got %q", got[0].Content)
	}
	if got[len(got)-1].Content != "new-9" {
		t.Fatalf("expected latest selected message last, got %q", got[len(got)-1].Content)
	}
}

func TestTruncateMessagesDoesNotSummarizeOnHotPathByDefault(t *testing.T) {
	t.Setenv("SELFMIND_SYNC_CONTEXT_SUMMARY", "")

	provider := &contextEngineProvider{}
	engine := NewContextEngine(120, 10)
	engine.SetProvider(provider)

	messages := []llm.Message{{Role: "system", Content: "system"}}
	for i := 0; i < 20; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: "long message with enough repeated words to exceed the small context window"})
	}

	got := engine.TruncateMessages(messages)
	if provider.calls.Load() != 0 {
		t.Fatalf("expected no synchronous summarization calls, got %d", provider.calls.Load())
	}
	if len(got) >= len(messages) {
		t.Fatalf("expected deterministic trimming, got %d messages from %d", len(got), len(messages))
	}
	if got[0].Role != "system" {
		t.Fatalf("expected system prompt to be preserved, got role %q", got[0].Role)
	}
}

func TestRecoverMessagesDropsOptionalProjectContextAndKeepsLatestUser(t *testing.T) {
	engine := NewContextEngine(240, 20)
	messages := []llm.Message{
		{Role: "system", Content: "core tool contract\n\n# PROJECT CONTEXT\n" + strings.Repeat("large project rules ", 200)},
		{Role: "user", Content: "old question " + strings.Repeat("old ", 120)},
		{Role: "assistant", Content: strings.Repeat("old answer ", 120)},
		{Role: "user", Content: "LATEST-INSTRUCTION"},
	}
	got := engine.RecoverMessages(messages)
	if len(got) == 0 || got[len(got)-1].Content != "LATEST-INSTRUCTION" {
		t.Fatalf("latest user instruction was not preserved: %+v", got)
	}
	if strings.Contains(got[0].Content, strings.Repeat("large project rules ", 20)) {
		t.Fatal("optional project context was not removed during recovery")
	}
	if !strings.Contains(got[0].Content, "core tool contract") {
		t.Fatal("core system contract was lost")
	}
}
