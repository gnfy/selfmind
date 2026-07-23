package memory

import (
	"strings"
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

func TestSelectFactsForPromptBoostsRelevantChineseMemory(t *testing.T) {
	now := time.Now()
	facts := []Fact{
		{Content: "User prefers concise technical explanations.", Confidence: 0.95, Scope: "global", CreatedAt: now},
		{Content: "日报应按分类标题和编号事项输出，每个事项一行。", Category: "communication", Confidence: 0.65, Scope: "global", CreatedAt: now},
	}

	got := SelectFactsForPrompt(facts, "global", "整理今天的发布日报，保持之前的格式", now, 1)
	if len(got) != 1 || !strings.Contains(got[0].Content, "日报") {
		t.Fatalf("topic-relevant memory should win the bounded prompt slot, got %+v", got)
	}
}

func TestSelectFactsForPromptFallsBackToGovernanceRanking(t *testing.T) {
	now := time.Now()
	facts := []Fact{
		{Content: "low", Confidence: 0.5, Scope: "global", CreatedAt: now},
		{Content: "high", Confidence: 0.9, Scope: "global", CreatedAt: now},
	}
	got := SelectFactsForPrompt(facts, "global", "unrelated topic", now, 1)
	if len(got) != 1 || got[0].Content != "high" {
		t.Fatalf("unrelated queries should preserve governance ordering, got %+v", got)
	}
}
