package memory

import (
	"strings"
	"testing"
	"time"
)

func TestBuildConsolidationDryRunKeepsScopeBoundaries(t *testing.T) {
	facts := []Fact{
		{ID: "a", Target: "memory", Scope: "workspace:game", Content: "Validate inline JavaScript with node --check.", Source: SourceFactExtractor},
		{ID: "b", Target: "memory", Scope: "workspace:game", Content: "Inline JavaScript is validated with node --check.", Source: SourceTurnExtractor},
		{ID: "c", Target: "memory", Scope: "workspace:selfmind", Content: "Validate inline JavaScript with node --check.", Source: SourceFactExtractor},
	}
	report := BuildConsolidationDryRun(facts, ConsolidationDryRunConfig{CandidateSimilarity: 0.35}, time.Now())
	if len(report.CandidateClusters) != 1 {
		t.Fatalf("clusters=%d, want 1: %+v", len(report.CandidateClusters), report.CandidateClusters)
	}
	cluster := report.CandidateClusters[0]
	if cluster.Scope != "workspace:game" || len(cluster.Members) != 2 {
		t.Fatalf("cross-scope or missing candidate: %+v", cluster)
	}
}

func TestBuildConsolidationDryRunFindsChineseParaphrases(t *testing.T) {
	facts := []Fact{
		{ID: "a", Target: "user", Scope: "global", Content: "用户偏好使用中文讨论技术问题。", Source: SourceFactExtractor},
		{ID: "b", Target: "user", Scope: "global", Content: "技术讨论时优先使用中文。", Source: SourceTurnExtractor},
		{ID: "c", Target: "user", Scope: "global", Content: "用户每天跑步五公里。", Source: SourceTurnExtractor},
	}
	report := BuildConsolidationDryRun(facts, ConsolidationDryRunConfig{CandidateSimilarity: 0.28}, time.Now())
	if len(report.CandidateClusters) != 1 || len(report.CandidateClusters[0].Members) != 2 {
		t.Fatalf("Chinese paraphrase candidate not isolated: %+v", report.CandidateClusters)
	}
}

func TestConsolidationProtectedFactsCannotBeMutated(t *testing.T) {
	facts := []Fact{
		{ID: "confirmed", Target: "user", Scope: "global", Content: "Always answer technical questions in Chinese.", Source: SourceUser},
		{ID: "auto", Target: "user", Scope: "global", Content: "Technical answers should always be in Chinese.", Source: SourceFactExtractor},
	}
	report := BuildConsolidationDryRun(facts, ConsolidationDryRunConfig{CandidateSimilarity: 0.2}, time.Now())
	if len(report.CandidateClusters) != 1 || !report.CandidateClusters[0].Protected {
		t.Fatalf("protected cluster missing: %+v", report.CandidateClusters)
	}
	decision := ConsolidationDecision{
		ClusterID: report.CandidateClusters[0].ID,
		Action:    "MERGE", Canonical: "Answer in Chinese.", Confidence: 0.99,
	}
	if err := ValidateConsolidationDecision(report, decision); err == nil || !strings.Contains(err.Error(), "cannot be changed") {
		t.Fatalf("protected merge accepted: %v", err)
	}
	decision.Action = "CONFLICT"
	decision.Canonical = ""
	if err := ValidateConsolidationDecision(report, decision); err != nil {
		t.Fatalf("protected conflict report rejected: %v", err)
	}
}

func TestArchiveCandidatesExcludeProtectedFacts(t *testing.T) {
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	old := now.Add(-200 * 24 * time.Hour)
	facts := []Fact{
		{ID: "old-auto", Target: "memory", Scope: "workspace:game", Content: "Old automatic fact", Source: SourceFactExtractor, CreatedAt: old},
		{ID: "old-user", Target: "user", Scope: "global", Content: "Old confirmed fact", Source: SourceUser, CreatedAt: old},
		{ID: "old-pin", Target: "pinned", Scope: "global", Content: "Old pinned fact", CreatedAt: old},
	}
	report := BuildConsolidationDryRun(facts, ConsolidationDryRunConfig{ArchiveAfter: 180 * 24 * time.Hour}, now)
	if len(report.ArchiveCandidates) != 1 || report.ArchiveCandidates[0].Fact.ID != "old-auto" {
		t.Fatalf("archive candidates=%+v", report.ArchiveCandidates)
	}
	if report.ProtectedFacts != 2 {
		t.Fatalf("protected=%d, want 2", report.ProtectedFacts)
	}
}

func TestValidateConsolidationDecisionRejectsForeignMember(t *testing.T) {
	facts := []Fact{
		{ID: "a", Target: "memory", Scope: "workspace:game", Content: "Use node check for JavaScript."},
		{ID: "b", Target: "memory", Scope: "workspace:game", Content: "Use node check when validating JavaScript."},
	}
	report := BuildConsolidationDryRun(facts, ConsolidationDryRunConfig{CandidateSimilarity: 0.2}, time.Now())
	decision := ConsolidationDecision{
		ClusterID: report.CandidateClusters[0].ID,
		Action:    "MERGE", Canonical: "Validate JavaScript with node check.", Confidence: 0.9,
		MemberIDs: []string{"a", "outside"},
	}
	if err := ValidateConsolidationDecision(report, decision); err == nil {
		t.Fatal("foreign member was accepted")
	}
}
