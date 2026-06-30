package memory

import (
	"testing"
	"time"
)

// TestMemoryEvalGuarantees encodes the "越用越放心" guarantees as regression
// guards over the scoring/selection pipeline (W3f). Each sub-test is a named
// behavioral guarantee; a failure means memory selection regressed.
func TestMemoryEvalGuarantees(t *testing.T) {
	now := time.Now()

	t.Run("user-stated outranks turn-extracted (no short-term-as-durable)", func(t *testing.T) {
		facts := []Fact{
			{Content: "turn", Source: SourceTurnExtractor, Confidence: BaseConfidence(SourceTurnExtractor), CreatedAt: now},
			{Content: "user", Source: SourceUser, Confidence: BaseConfidence(SourceUser), CreatedAt: now},
		}
		if got := SelectFacts(facts, "global", now, 1); got[0].Content != "user" {
			t.Fatalf("user-stated fact should win, got %q", got[0].Content)
		}
	})

	t.Run("fresh high-confidence beats stale low-confidence", func(t *testing.T) {
		facts := []Fact{
			{Content: "stale-low", Confidence: 0.5, CreatedAt: now.Add(-200 * 24 * time.Hour)},
			{Content: "fresh-high", Confidence: 0.9, CreatedAt: now},
		}
		if got := SelectFacts(facts, "global", now, 1); got[0].Content != "fresh-high" {
			t.Fatalf("fresh high-confidence should win, got %q", got[0].Content)
		}
	})

	t.Run("workspace fact preferred in-workspace, deprioritized elsewhere", func(t *testing.T) {
		ws := Fact{Content: "ws", Confidence: 0.6, Scope: "workspace:A", CreatedAt: now}
		glob := Fact{Content: "glob", Confidence: 0.6, Scope: "global", CreatedAt: now}
		if got := SelectFacts([]Fact{glob, ws}, "workspace:A", now, 1); got[0].Content != "ws" {
			t.Fatalf("in workspace A, the A-scoped fact should win, got %q", got[0].Content)
		}
		if got := SelectFacts([]Fact{glob, ws}, "workspace:B", now, 1); got[0].Content != "glob" {
			t.Fatalf("in workspace B, the global fact should win, got %q", got[0].Content)
		}
	})

	t.Run("legacy (unscored) facts are retained", func(t *testing.T) {
		facts := []Fact{
			{Content: "legacy1", CreatedAt: now.Add(-time.Hour)},
			{Content: "legacy2", CreatedAt: now.Add(-2 * time.Hour)},
		}
		if got := SelectFacts(facts, "global", now, 5); len(got) != 2 {
			t.Fatalf("legacy facts must not be dropped, got %d", len(got))
		}
	})
}

// TestMemoryEvalProviderRoundTrip exercises the full write→read→select pipeline
// through the real SQLite provider: a user-stated fact and a turn-extracted fact
// persisted with metadata, then the user-stated one is selected first.
func TestMemoryEvalProviderRoundTrip(t *testing.T) {
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	mustAdd := func(content, source string) {
		if err := p.AddFactMeta(nil, "t1", Fact{
			Target: "user", Content: content, Source: source, Scope: "global", Confidence: BaseConfidence(source),
		}); err != nil {
			t.Fatalf("AddFactMeta: %v", err)
		}
	}
	mustAdd("casual aside", SourceTurnExtractor)
	mustAdd("prefers tabs", SourceUser)

	facts, err := p.GetFacts(nil, "t1", "user")
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	got := SelectFacts(facts, "global", time.Now(), 1)
	if len(got) != 1 || got[0].Content != "prefers tabs" {
		t.Fatalf("user-stated fact should be selected first, got %+v", got)
	}
}
