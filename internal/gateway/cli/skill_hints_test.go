package cli

import (
	"strings"
	"testing"

	"selfmind/internal/tools"
)

func completionModel(candidates ...tools.SkillCompletionCandidate) *uiModel {
	return &uiModel{skillCompletion: candidates}
}

// The popup shows the label and writes the reference that resolves it.
func TestSkillCompletionHintsShapeRows(t *testing.T) {
	m := completionModel(tools.SkillCompletionCandidate{
		Name: "grilling", Qualified: "external:grilling", Label: "grilling",
		Description: "stress-test a plan", Provenance: "external",
		Reference: "external:grilling",
	})

	hints := m.skillCompletionHints("")
	if len(hints) != 1 {
		t.Fatalf("unexpected hints: %+v", hints)
	}
	if hints[0].Name != "$grilling" {
		t.Fatalf("row label = %q", hints[0].Name)
	}
	if hints[0].Insert != "/external:grilling" {
		t.Fatalf("insertion = %q", hints[0].Insert)
	}
	if !strings.Contains(hints[0].Description, "external") {
		t.Fatalf("description should name the origin: %q", hints[0].Description)
	}
}

// With nothing typed the inventory is ordered by usage recency, which is how
// implicit use reaches the popup without touching catalog ranking.
func TestSkillCompletionHintsOrderByRecencyWhenNothingTyped(t *testing.T) {
	m := completionModel(
		tools.SkillCompletionCandidate{Name: "alpha", Qualified: "user:alpha", Label: "alpha", Reference: "user:alpha"},
		tools.SkillCompletionCandidate{Name: "beta", Qualified: "user:beta", Label: "beta", Reference: "user:beta", LastUsed: "2026-08-27T10:00:00Z"},
		tools.SkillCompletionCandidate{Name: "gamma", Qualified: "user:gamma", Label: "gamma", Reference: "user:gamma", LastUsed: "2026-08-28T10:00:00Z"},
	)

	hints := m.skillCompletionHints("")
	if len(hints) != 3 {
		t.Fatalf("unexpected hints: %+v", hints)
	}
	if hints[0].Name != "$gamma" || hints[1].Name != "$beta" || hints[2].Name != "$alpha" {
		t.Fatalf("recency order not applied: %+v", hints)
	}
}

// A typed query goes through the shared metadata ranker, so completion drops
// non-matches instead of prefix-testing.
func TestSkillCompletionHintsRankTypedQuery(t *testing.T) {
	m := completionModel(
		tools.SkillCompletionCandidate{Name: "grilling", Qualified: "user:grilling", Label: "grilling", Description: "stress-test a plan", Reference: "user:grilling"},
		tools.SkillCompletionCandidate{Name: "release-flow", Qualified: "user:release-flow", Label: "release-flow", Description: "ship a release", Reference: "user:release-flow"},
	)

	hints := m.skillCompletionHints("release")
	if len(hints) != 1 || hints[0].Name != "$release-flow" {
		t.Fatalf("ranked hints = %+v", hints)
	}
}

// The popup promises the whole inventory, so a command that can add or remove a
// Skill must trigger a refresh.
func TestInventoryChangingCommandsTriggerRefresh(t *testing.T) {
	for _, command := range []string{"/skills", "/reload-skills", "/bundles"} {
		if !skillCommandMayChangeInventory(command) {
			t.Fatalf("%s should refresh the inventory", command)
		}
	}
	for _, command := range []string{"/help", "/status", "/memory"} {
		if skillCommandMayChangeInventory(command) {
			t.Fatalf("%s should not refresh the inventory", command)
		}
	}
}
