package cli

// Live token ticking: token.updated events must move the status bar's run
// counter DURING the run, while the final MsgAgentDone usage stays the single
// authoritative session increment (no double counting).

import (
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestTokenEventUpdatesRunTokensMidRun(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.totalTokens = 1000

	updated, _ := model.Update(MsgTokens{Run: 250})
	m := updated.(*uiModel)
	if m.runTokens != 250 {
		t.Fatalf("runTokens = %d, want 250", m.runTokens)
	}
	if m.totalTokens != 1000 {
		t.Fatalf("live tick must not touch session totals, got %d", m.totalTokens)
	}

	// Later cumulative snapshot replaces (not adds to) the run counter.
	updated, _ = m.Update(MsgTokens{Run: 700})
	m = updated.(*uiModel)
	if m.runTokens != 700 {
		t.Fatalf("runTokens = %d, want 700", m.runTokens)
	}

	// A zero/empty snapshot never resets a live counter mid-run.
	updated, _ = m.Update(MsgTokens{Run: 0})
	m = updated.(*uiModel)
	if m.runTokens != 700 {
		t.Fatalf("zero snapshot must not reset runTokens, got %d", m.runTokens)
	}

	// Final response usage stays authoritative: it overwrites runTokens and
	// increments the session total exactly once.
	updated, _ = m.Update(MsgAgentDone{Response: "done", Usage: llm.UsageStats{InputTokens: 500, OutputTokens: 300}})
	m = updated.(*uiModel)
	if m.runTokens != 800 {
		t.Fatalf("final usage should own runTokens, got %d", m.runTokens)
	}
	if m.totalTokens != 1800 {
		t.Fatalf("session total should increment once by final usage, got %d", m.totalTokens)
	}
}

func TestRunTokensFromEvent(t *testing.T) {
	// Daemon-client path: typed Usage snapshot.
	usage := llm.StreamEvent{EventType: "token.updated", Usage: &llm.UsageStats{InputTokens: 1200, OutputTokens: 34}}
	if got := runTokensFromEvent(usage); got != 1234 {
		t.Fatalf("usage snapshot = %d, want 1234", got)
	}
	// In-process gateway path: raw payload (JSON numbers decode to float64).
	payload := llm.StreamEvent{EventType: "token.updated", Payload: map[string]interface{}{
		"input_tokens":  float64(40),
		"output_tokens": float64(2),
	}}
	if got := runTokensFromEvent(payload); got != 42 {
		t.Fatalf("payload snapshot = %d, want 42", got)
	}
	if got := runTokensFromEvent(llm.StreamEvent{EventType: "token.updated"}); got != 0 {
		t.Fatalf("empty event = %d, want 0", got)
	}
}
