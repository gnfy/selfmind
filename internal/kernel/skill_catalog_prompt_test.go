package kernel

import (
	"fmt"
	"strings"
	"testing"
)

func TestSkillCandidateCatalogPreservesExistenceBeforeDescriptions(t *testing.T) {
	candidates := make([]SkillCandidateContext, 0, 12)
	for i := 0; i < 12; i++ {
		candidates = append(candidates, SkillCandidateContext{
			Name: fmt.Sprintf("skill-%02d", i), Scope: "user", Source: "agent-created",
			Description: strings.Repeat(fmt.Sprintf("description-%02d ", i), 20),
		})
	}

	prompt, report := renderSkillCandidateCatalog(candidates, 1400)
	if report.Included != len(candidates) || report.Omitted != 0 {
		t.Fatalf("catalog lost existence before descriptions: %+v\n%s", report, prompt)
	}
	if report.Full == 0 || report.Shortened == 0 {
		t.Fatalf("expected ranked full descriptions plus fair shortened descriptions under budget: %+v", report)
	}
	if len(prompt) > 1400 {
		t.Fatalf("catalog bytes = %d, want <= 1400", len(prompt))
	}
	for _, candidate := range candidates {
		if !strings.Contains(prompt, "- "+candidate.Name) {
			t.Fatalf("catalog omitted %s despite sufficient minimum-line budget:\n%s", candidate.Name, prompt)
		}
	}
	for _, want := range []string{"included 12/12", "full descriptions", "shortened", "omitted 0"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("catalog report missing %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, strings.TrimSpace(candidates[0].Description)) {
		t.Fatalf("highest-ranked candidate did not receive a full description:\n%s", prompt)
	}
}

func TestSkillCandidateCatalogUsesFullDescriptionsWhenTheyFit(t *testing.T) {
	candidates := []SkillCandidateContext{
		{Name: "release", Description: strings.Repeat("full description ", 30), Scope: "user", Source: "manual"},
		{Name: "review", Description: "second complete description", Scope: "user", Source: "manual"},
	}
	prompt, report := renderSkillCandidateCatalog(candidates, 2000)
	if report.Full != 2 || report.Shortened != 0 || report.Omitted != 0 {
		t.Fatalf("descriptions that fit were not delivered in full: %+v\n%s", report, prompt)
	}
	for _, candidate := range candidates {
		if !strings.Contains(prompt, strings.TrimSpace(candidate.Description)) {
			t.Fatalf("catalog pre-truncated %q despite sufficient budget:\n%s", candidate.Name, prompt)
		}
	}
}

func TestSkillCandidateCatalogShowsScopeOnlyForAmbiguousNames(t *testing.T) {
	candidates := []SkillCandidateContext{
		{Name: "unique", Description: "one", Scope: "workspace", Source: "manual"},
		{Name: "same", Description: "two", Scope: "workspace", Source: "manual"},
		{Name: "same", Description: "three", Scope: "user", Source: "agent-created"},
	}
	prompt, _ := renderSkillCandidateCatalog(candidates, 2000)
	if strings.Contains(prompt, "unique [workspace/manual]") {
		t.Fatalf("non-ambiguous candidate leaked scope/source:\n%s", prompt)
	}
	if !strings.Contains(prompt, "same [workspace/manual]") || !strings.Contains(prompt, "same [user/agent-created]") {
		t.Fatalf("ambiguous candidates lost disambiguation:\n%s", prompt)
	}
}

func TestSkillCandidateCatalogNeverRendersDaemonRoot(t *testing.T) {
	prompt, _ := renderSkillCandidateCatalog([]SkillCandidateContext{{
		CandidateRef: "skref_1", Name: "inspect", Description: "Inspect releases",
		Scope: "user", Source: "agent-created", Root: "/private/control/tenant/skills",
	}}, 2000)
	if strings.Contains(prompt, "/private/control") {
		t.Fatalf("daemon Skill root leaked into model catalog:\n%s", prompt)
	}
}

func TestSkillCandidateCatalogOmissionIsDeterministic(t *testing.T) {
	candidates := []SkillCandidateContext{
		{Name: "ranked-first", Scope: "user", Source: "manual"},
		{Name: "ranked-second", Scope: "user", Source: "manual"},
		{Name: "ranked-third", Scope: "user", Source: "manual"},
	}
	first, firstReport := renderSkillCandidateCatalog(candidates, 330)
	second, secondReport := renderSkillCandidateCatalog(candidates, 330)
	if first != second || firstReport != secondReport {
		t.Fatalf("catalog allocation is not deterministic:\n%+v\n%+v", firstReport, secondReport)
	}
	if firstReport.Omitted == 0 || !strings.Contains(first, "ranked-first") {
		t.Fatalf("small budget did not preserve ranked order: %+v\n%s", firstReport, first)
	}
}

func TestSkillCandidateCatalogEnforcesEstimatedTokensAndExactBytes(t *testing.T) {
	candidates := []SkillCandidateContext{{Name: "unicode", Description: strings.Repeat("说明", 800)}}
	prompt, report := renderSkillCandidateCatalogWithinBudget(candidates, 2000, 120)
	if len(prompt) > 2000 || estimateTokens(prompt) > 120 || !report.WithinBudget() {
		t.Fatalf("dual budget exceeded: report=%+v bytes=%d tokens=%d", report, len(prompt), estimateTokens(prompt))
	}
}

func TestSkillCatalogRenderReportWithinBudgetChecksTokens(t *testing.T) {
	report := SkillCatalogRenderReport{Bytes: 100, Budget: 100, Tokens: 121, TokenBudget: 120}
	if report.WithinBudget() {
		t.Fatalf("token overflow was reported within budget: %+v", report)
	}
	report.Tokens = 120
	if !report.WithinBudget() {
		t.Fatalf("exact dual-budget fit was rejected: %+v", report)
	}
}

func TestSkillCandidateCatalogCapsIssuedSurfaceAtWorkUnitLimit(t *testing.T) {
	candidates := make([]SkillCandidateContext, SkillCatalogCandidateLimit+20)
	for index := range candidates {
		candidates[index] = SkillCandidateContext{Name: fmt.Sprintf("s%03d", index)}
	}
	_, report := renderSkillCandidateCatalogWithinBudget(candidates, 20000, 10000)
	if report.Included != SkillCatalogCandidateLimit || report.Omitted != 20 {
		t.Fatalf("candidate limit not enforced: %+v", report)
	}
}
