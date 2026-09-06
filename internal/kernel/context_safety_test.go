package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

func TestContextCountsNativeToolArguments(t *testing.T) {
	engine := NewContextEngine(2048, 256)
	plain := []llm.Message{{Role: "system", Content: "sys"}, {Role: "assistant", Content: ""}}
	rich := append([]llm.Message(nil), plain...)
	rich[1].ToolCalls = []llm.ToolCall{{ID: "review-call", Function: "write_file", Args: `{"path":"output.txt","content":"` + strings.Repeat("payload ", 8000) + `"}`}}
	before, after := engine.countMessages(plain), engine.countMessages(rich)
	if after <= before {
		t.Fatalf("budget ignores native tool arguments: plain=%d with 64KB arguments=%d", before, after)
	}
}

func TestContextFallbackPreservesGoal(t *testing.T) {
	engine := NewContextEngine(1024, 128) // no summarizer: production fallback when unavailable
	msgs := []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "GOAL_SENTINEL: repair the import flow; do not publish"}}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("call-%d", i)
		msgs = append(msgs, llm.Message{Role: "assistant", Content: "inspect next", ToolCalls: []llm.ToolCall{{ID: id, Function: "read_file", Args: `{"path":"source.txt"}`}}}, llm.Message{Role: "tool", Content: strings.Repeat("tool observation ", 180), ToolCallID: id})
	}
	trimmed := engine.TruncateMessagesCtx(context.Background(), msgs)
	foundGoal := false
	calls := make(map[string]bool)
	for _, m := range trimmed {
		foundGoal = foundGoal || strings.Contains(m.Content, "GOAL_SENTINEL")
		for _, call := range m.ToolCalls {
			calls[call.ID] = true
		}
		if m.Role == "tool" {
			if !calls[m.ToolCallID] {
				t.Fatal("trim orphaned a tool result")
			}
			delete(calls, m.ToolCallID)
		}
	}
	if !foundGoal || len(calls) != 0 || len(trimmed) >= len(msgs) {
		t.Fatalf("trim lost the goal or tool pairing, or did not reduce history: goal=%v pending=%v remaining=%d", foundGoal, calls, len(trimmed))
	}
}

func TestRequestBudgetIncludesToolCatalog(t *testing.T) {
	engine := NewContextEngine(2048, 256)
	definitions := []llm.ToolDefinition{{Name: "read_file", Description: strings.Repeat("tool documentation ", 300), Parameters: map[string]interface{}{"type": "object"}}}
	goal := "current goal: fix source; never publish"
	ctx := withContextInput(context.Background(), goal)
	messages := []llm.Message{{Role: "system", Content: "system"}, {Role: "user", Content: strings.Repeat("older history ", 600)}, {Role: "user", Content: goal}}
	prepared, err := engine.PrepareRequest(ctx, messages, definitions)
	if err != nil {
		t.Fatal(err)
	}
	if engine.countMessages(prepared)+engine.tokenizer.CountTools(definitions) > engine.inputBudget() || len(prepared) >= len(messages) {
		t.Fatal("tool catalog did not reduce the available history budget")
	}
	if prepared[len(prepared)-1].Content != goal {
		t.Fatal("current goal lost behind older spine input")
	}
	oversized := []llm.Message{{Role: "user", Content: strings.Repeat("mandatory instruction ", 3000)}}
	if _, err := engine.PrepareRequest(context.Background(), oversized, nil); err == nil {
		t.Fatal("oversized mandatory input should fail before provider dispatch")
	}
}

func TestCompactionRetainsCurrentInputAndSteering(t *testing.T) {
	engine := NewContextEngine(800, 128)
	provider := &fakeSummarizer{reply: "## Active Task\nContinue the repair."}
	engine.SetSummaryProvider(provider)
	goal, guidance := "repair source", "do not publish; inspect only"
	ctx := withContextInput(context.Background(), goal)
	messages := []llm.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "an unrelated old goal"}, {Role: "assistant", Content: "old answer"}, {Role: "user", Content: goal}, {Role: "user", Content: guidance}}
	for i := 0; i < 12; i++ {
		messages = append(messages, llm.Message{Role: "assistant", Content: strings.Repeat("observation ", 200)})
	}
	for _, method := range []string{"summary", "fallback", "recovery"} {
		engine.SetSummaryProvider(provider)
		if method == "fallback" {
			engine.SetSummaryProvider(nil)
		}
		var got []llm.Message
		if method == "recovery" {
			got = engine.RecoverMessagesCtx(ctx, messages)
		} else {
			got = engine.TruncateMessagesCtx(ctx, messages)
		}
		for _, required := range []string{goal, guidance} {
			found := false
			for _, message := range got {
				found = found || message.Content == required
			}
			if !found {
				t.Fatalf("%s lost protected input %q", method, required)
			}
		}
	}
}

func TestSummaryInheritsOwnerAndDeadline(t *testing.T) {
	p := &contextReviewSummarizer{fakeSummarizer: &fakeSummarizer{reply: "summary"}}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(p)
	ctx, cancel := context.WithTimeout(llm.WithModelContext(context.Background(), llm.ModelContext{PersonID: "person", RunID: "run"}), time.Second)
	defer cancel()
	engine.TruncateMessagesCtx(ctx, compactionFixture())
	if p.calls.Load() != 1 || p.owner.PersonID != "person" || p.owner.RunID != "run" || p.owner.Role != llm.RoleSummarizer || !p.deadline {
		t.Fatalf("summary lost owner or timeout: %+v deadline=%v", p.owner, p.deadline)
	}
}

func TestResumedContextKeepsOriginalConstraints(t *testing.T) {
	engine := NewContextEngine(800, 128)
	goal := "repair source; do not publish"
	checkpoint := []llm.Message{{Role: "user", Content: goal}}
	for i := 0; i < 10; i++ {
		checkpoint = append(checkpoint, llm.Message{Role: "assistant", Content: strings.Repeat("observation ", 200)})
	}
	ctx := withContextInput(WithLoopResumeMessages(context.Background(), checkpoint), "continue")
	messages := append(cloneLoopMessages(checkpoint), llm.Message{Role: "user", Content: "continue"})
	prepared, err := engine.PrepareRequest(ctx, messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared[0].Content != goal || prepared[len(prepared)-1].Content != "continue" {
		t.Fatal("resume discarded the original goal or current continuation")
	}
}

func TestTaskContextDeduplicatesSummaryAndNextSteps(t *testing.T) {
	runtime := TaskRuntimeContext{Summary: "The same summary.", NextSteps: []string{"verify change"}, Handoff: &TaskHandoffContext{Summary: "The same summary.", NextSteps: []string{"verify change", "inspect result"}}}
	prompt := runtime.Prompt(4000)
	for _, text := range []string{"The same summary.", "verify change", "inspect result"} {
		if strings.Count(prompt, text) != 1 {
			t.Fatalf("expected one copy of %q", text)
		}
	}
}

type contextReviewSummarizer struct {
	*fakeSummarizer
	owner     llm.ModelContext
	cancelled bool
	deadline  bool
}

func (p *contextReviewSummarizer) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.owner = llm.ModelContextFrom(ctx)
	p.cancelled = ctx.Err() != nil
	_, p.deadline = ctx.Deadline()
	return p.fakeSummarizer.Chat(ctx, req)
}

func TestCancelledRunSkipsSummary(t *testing.T) {
	p := &contextReviewSummarizer{fakeSummarizer: &fakeSummarizer{reply: "## Active Task\nbuild app"}}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(p)
	ctx, cancel := context.WithCancel(llm.WithModelContext(context.Background(), llm.ModelContext{TenantID: "review-tenant", PersonID: "review-person", RunID: "review-run"}))
	cancel()
	engine.TruncateMessagesCtx(ctx, compactionFixture())
	if p.calls.Load() != 0 {
		t.Fatal("cancelled Run dispatched a summary request")
	}
}

func TestTaskSliceRetainsVerification(t *testing.T) {
	summary := strings.Repeat("Investigated current behavior. ", 35)
	runtime := TaskRuntimeContext{TaskID: "task", RunID: "run", Summary: summary, NextSteps: []string{strings.Repeat("next action ", 30)}, Handoff: &TaskHandoffContext{Summary: summary, TestStatus: "VERIFICATION_SENTINEL: integration tests FAILED", Risks: []string{"do not deploy before verification"}}}
	budget := DefaultRuntimeContextBudget().TaskChars
	prompt := runtime.Prompt(budget)
	if !strings.Contains(prompt, "VERIFICATION_SENTINEL") {
		t.Fatalf("task slice (%d bytes budget) spent on duplicated summary and omitted verification", budget)
	}
}

// summaryCancelProbe cancels the RUN while the summary request is in flight and
// records whether the request's own context noticed.
type summaryCancelProbe struct {
	*fakeSummarizer
	cancelRun     func()
	sawRunCancel  bool
	deadlineAfter time.Duration
}

func (p *summaryCancelProbe) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlineAfter = time.Until(deadline)
	}
	p.cancelRun()
	// Cancellation propagates through the context tree, not instantly across
	// goroutines; give it a bounded moment rather than racing it.
	for i := 0; i < 100 && ctx.Err() == nil; i++ {
		time.Sleep(time.Millisecond)
	}
	p.sawRunCancel = ctx.Err() != nil
	return p.fakeSummarizer.Chat(ctx, req)
}

// TestSummaryStopsWhenTheRunIsCancelledMidFlight closes the half the existing
// tests leave open. One cancels BEFORE the call, so an outer guard catches it
// and the request context's origin never matters; the other asserts only that
// SOME deadline exists, which a context built from Background also has. Neither
// notices if the summary stops descending from the run — and a summary that
// outlives the run it belongs to is exactly what that break would cause.
func TestSummaryStopsWhenTheRunIsCancelledMidFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(llm.WithModelContext(context.Background(),
		llm.ModelContext{PersonID: "person", RunID: "run"}))
	defer cancel()

	probe := &summaryCancelProbe{
		fakeSummarizer: &fakeSummarizer{reply: "summary"},
		cancelRun:      cancel,
	}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(probe)
	engine.TruncateMessagesCtx(ctx, compactionFixture())

	if probe.calls.Load() == 0 {
		t.Fatal("fixture error: the summary was never dispatched")
	}
	if !probe.sawRunCancel {
		t.Fatal("cancelling the run did not reach the in-flight summary; the summary no longer descends from the run's context")
	}
}

// TestSummaryDeadlineIsBoundedByTheRun: a run whose own deadline is nearer than
// the summary's own bound must win, or a summary can outlive its run.
func TestSummaryDeadlineIsBoundedByTheRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(llm.WithModelContext(context.Background(),
		llm.ModelContext{PersonID: "person", RunID: "run"}), 250*time.Millisecond)
	defer cancel()

	probe := &summaryCancelProbe{fakeSummarizer: &fakeSummarizer{reply: "summary"}, cancelRun: func() {}}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(probe)
	engine.TruncateMessagesCtx(ctx, compactionFixture())

	if probe.calls.Load() == 0 {
		t.Fatal("fixture error: the summary was never dispatched")
	}
	if probe.deadlineAfter <= 0 || probe.deadlineAfter > time.Second {
		t.Fatalf("summary deadline = %v; it must inherit the run's nearer bound, not its own 30s ceiling",
			probe.deadlineAfter)
	}
}
