package httpapi

import (
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestEventTypeWeightOrdering(t *testing.T) {
	if !(eventTypeWeight("run.outcome") > eventTypeWeight("tool.completed") &&
		eventTypeWeight("tool.completed") > eventTypeWeight("agent.thinking")) {
		t.Fatal("expected outcome > tool > thinking")
	}
	if eventTypeWeight("error.something") < 0.9 {
		t.Fatal("error events should rank high")
	}
}

func TestRankTaskEventsKeepsImportantOverRecentNoise(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	ev := func(typ string, minutes int) control.Event {
		return control.Event{Type: typ, CreatedAt: base.Add(time.Duration(minutes) * time.Minute)}
	}
	// One important early event, then a burst of recent low-value noise.
	events := []control.Event{
		ev("run.outcome", 1),
		ev("handoff", 2),
		ev("agent.thinking", 50),
		ev("agent.thinking", 51),
		ev("stream", 52),
		ev("agent.step", 53),
		ev("stream", 54),
	}

	ranked := rankTaskEvents(events, 3)
	if len(ranked) != 3 {
		t.Fatalf("expected 3 ranked events, got %d", len(ranked))
	}
	kinds := map[string]bool{}
	for _, e := range ranked {
		kinds[e.Type] = true
	}
	if !kinds["run.outcome"] || !kinds["handoff"] {
		t.Fatalf("important early events should survive the budget: %+v", kinds)
	}
	// Output must be chronological.
	for i := 1; i < len(ranked); i++ {
		if ranked[i].CreatedAt.Before(ranked[i-1].CreatedAt) {
			t.Fatal("ranked events should be returned in chronological order")
		}
	}
}

func TestRankTaskEventsUnderBudgetSortsChronologically(t *testing.T) {
	base := time.Now()
	events := []control.Event{
		{Type: "b", CreatedAt: base.Add(2 * time.Minute)},
		{Type: "a", CreatedAt: base.Add(1 * time.Minute)},
	}
	out := rankTaskEvents(events, 8)
	if len(out) != 2 || out[0].Type != "a" || out[1].Type != "b" {
		t.Fatalf("under budget should keep all, chronological: %+v", out)
	}
}

func TestRankTaskEventsExcludesRecallUsageTelemetry(t *testing.T) {
	now := time.Now()
	out := rankTaskEvents([]control.Event{
		{Type: "context.recall_usage", CreatedAt: now},
		{Type: "run.outcome", CreatedAt: now.Add(time.Second)},
	}, 8)
	if len(out) != 1 || out[0].Type != "run.outcome" {
		t.Fatalf("ranked events = %+v", out)
	}
}
