package modelchange

import (
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestBuildCandidatePreservesNonModelTuningAndCompatibleReasoning(t *testing.T) {
	current := Snapshot{
		Primary: config.ModelSelectionConfig{
			Provider: "codex-cli", Model: "gpt-5.5", Reasoning: "high",
			ServiceTier: "priority", ContextLength: 424242,
		},
		Auxiliary: config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-5.5", Reasoning: "low"},
	}
	result, err := BuildCandidate(current, SelectionPatch{
		Route: RoutePrimary, Provider: "codex-cli", Model: "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Primary.Reasoning != "high" {
		t.Fatalf("reasoning = %q, notices=%v", result.Snapshot.Primary.Reasoning, result.Notices)
	}
	if result.Snapshot.Primary.ServiceTier != "priority" || result.Snapshot.Primary.ContextLength != 424242 {
		t.Fatalf("primary tuning was cleared: %+v", result.Snapshot.Primary)
	}
	if result.Snapshot.Auxiliary != current.Auxiliary {
		t.Fatalf("auxiliary changed: %+v", result.Snapshot.Auxiliary)
	}
}

func TestBuildCandidateExplicitAutoClearsReasoningWithoutClearingOtherFields(t *testing.T) {
	current := Snapshot{Auxiliary: config.ModelSelectionConfig{
		Provider: "codex-cli", Model: "gpt-5.5", Reasoning: "xhigh",
		ServiceTier: "priority", ContextLength: 99,
	}}
	result, err := BuildCandidate(current, SelectionPatch{
		Route: RouteAuxiliary, Provider: "anthropic", Model: "claude-new",
		Reasoning: OptionalValue{Set: true, Value: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Auxiliary.Reasoning != "" {
		t.Fatalf("reasoning = %q", result.Snapshot.Auxiliary.Reasoning)
	}
	if result.Snapshot.Auxiliary.ServiceTier != "" {
		// Unknown new-model metadata cannot prove that a model-specific tier is
		// compatible, so it intentionally returns to provider auto.
		t.Fatalf("service tier = %q, notices=%v", result.Snapshot.Auxiliary.ServiceTier, result.Notices)
	}
	if result.Snapshot.Auxiliary.ContextLength != 99 {
		t.Fatalf("context length = %d", result.Snapshot.Auxiliary.ContextLength)
	}
}

func TestBuildCandidateRejectsKnownUnsupportedExplicitTuning(t *testing.T) {
	current := Snapshot{Primary: config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-5.5"}}
	_, err := BuildCandidate(current, SelectionPatch{
		Route: RoutePrimary, Provider: "deepseek", Model: "deepseek-v4-preview",
		Reasoning: OptionalValue{Set: true, Value: "medium"},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildCandidateSupportsBackgroundRoleOverride(t *testing.T) {
	current := Snapshot{
		Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"},
		Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
	}
	result, err := BuildCandidate(current, SelectionPatch{
		Route: RouteMemoryExtract, Provider: "anthropic", Model: "claude-role",
		Reasoning: OptionalValue{Set: true, Value: "low"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := SelectionForRoute(result.Snapshot, RouteMemoryExtract)
	if got.Provider != "anthropic" || got.Model != "claude-role" || got.Reasoning != "low" {
		t.Fatalf("memory_extract = %+v", got)
	}
	if result.Snapshot.Primary != current.Primary || result.Snapshot.Auxiliary != current.Auxiliary {
		t.Fatalf("base routes changed: %+v", result.Snapshot)
	}
}

func TestResetRoleOverrideReturnsRoleToBackground(t *testing.T) {
	current := Snapshot{
		Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
	}
	current.Roles.MemoryExtract = config.ModelSelectionConfig{Provider: "anthropic", Model: "claude-role"}

	result, err := ResetRoleCandidate(current, RouteMemoryExtract)
	if err != nil {
		t.Fatal(err)
	}
	if got := SelectionForRoute(result.Snapshot, RouteMemoryExtract); got != (config.ModelSelectionConfig{}) {
		t.Fatalf("memory_extract override = %+v, want inherited background", got)
	}
}
