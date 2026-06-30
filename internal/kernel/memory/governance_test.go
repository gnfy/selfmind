package memory

import (
	"testing"
	"time"
)

func TestBaseConfidenceBySource(t *testing.T) {
	if !(BaseConfidence(SourceUser) > BaseConfidence(SourceFactExtractor) &&
		BaseConfidence(SourceFactExtractor) > BaseConfidence(SourceTurnExtractor)) {
		t.Fatal("expected user > fact_extractor > turn_extractor")
	}
	if BaseConfidence("unknown") != 0.5 {
		t.Fatalf("unknown source should default to 0.5")
	}
}

func TestEffectiveConfidenceDecayAndLegacy(t *testing.T) {
	if got := EffectiveConfidence(0, 0); got != 0.5 {
		t.Fatalf("legacy/unscored should be neutral 0.5, got %v", got)
	}
	if got := EffectiveConfidence(0.8, 0); got != 0.8 {
		t.Fatalf("fresh should equal stored, got %v", got)
	}
	if got := EffectiveConfidence(0.8, DecayHalfLife); got < 0.35 || got > 0.45 {
		t.Fatalf("one half-life should ~halve 0.8 → ~0.4, got %v", got)
	}
	if got := EffectiveConfidence(0.8, 100*DecayHalfLife); got < 0.2*0.8-1e-9 {
		t.Fatalf("must not decay below the 20%% floor, got %v", got)
	}
}

func TestDeriveFactScope(t *testing.T) {
	if DeriveFactScope("user", "ws1") != "global" {
		t.Fatal("user preferences are global")
	}
	if DeriveFactScope("memory", "ws1") != "workspace:ws1" {
		t.Fatal("environment facts scope to the workspace")
	}
	if DeriveFactScope("memory", "") != "global" {
		t.Fatal("no workspace → global")
	}
}

func TestSelectFactsRanksAndCaps(t *testing.T) {
	now := time.Now()
	facts := []Fact{
		{Content: "legacy", CreatedAt: now.Add(-time.Hour)},                                    // 0 → neutral 0.5×1.0
		{Content: "high-onws", Confidence: 0.9, Scope: "workspace:ws1", CreatedAt: now},        // 0.9×1.25
		{Content: "low-elsewhere", Confidence: 0.55, Scope: "workspace:other", CreatedAt: now}, // 0.55×0.6
	}

	top := SelectFacts(facts, "workspace:ws1", now, 2)
	if len(top) != 2 {
		t.Fatalf("cap not honored: %d", len(top))
	}
	if top[0].Content != "high-onws" {
		t.Fatalf("highest-confidence on-workspace fact should rank first, got %q", top[0].Content)
	}

	all := SelectFacts(facts, "workspace:ws1", now, 10)
	pos := map[string]int{}
	for i, f := range all {
		pos[f.Content] = i
	}
	if pos["legacy"] > pos["low-elsewhere"] {
		t.Fatalf("legacy (0.5) should outrank down-weighted other-workspace (0.33): %v", pos)
	}
}
