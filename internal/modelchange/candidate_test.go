package modelchange

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestBuildCandidatePreservesNonModelTuningAndCompatibleReasoning(t *testing.T) {
	installCodexCandidateCapabilities(t)
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

func TestBuildCandidateUnknownCapabilitiesResetModelSpecificTuning(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	current := Snapshot{Primary: config.ModelSelectionConfig{
		Provider: "codex-cli", Model: "gpt-old", Reasoning: "high",
		ServiceTier: "priority", ContextLength: 424242,
	}}
	result, err := BuildCandidate(current, SelectionPatch{
		Route: RoutePrimary, Provider: "codex-cli", Model: "future-model-without-metadata",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Primary.Reasoning != "" || result.Snapshot.Primary.ServiceTier != "" {
		t.Fatalf("unknown model kept model-specific tuning: %+v", result.Snapshot.Primary)
	}
	if result.Snapshot.Primary.ContextLength != 424242 {
		t.Fatalf("non-model tuning was cleared: %+v", result.Snapshot.Primary)
	}
	if len(result.Notices) != 2 {
		t.Fatalf("notices = %v, want reasoning and service-tier fallbacks", result.Notices)
	}
}

func installCodexCandidateCapabilities(t *testing.T) {
	t.Helper()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	data := []byte(`{
  "models": [{
    "slug": "gpt-5.6-sol",
    "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}],
    "service_tiers": [{"id": "priority"}]
  }]
}`)
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
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

func TestBuildCandidateCanDisableBackgroundWithoutProviderOrModel(t *testing.T) {
	current := Snapshot{
		Primary:   config.ModelSelectionConfig{Provider: "openai", Model: "gpt-main"},
		Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-chat"},
	}
	disabled := false
	result, err := BuildCandidate(current, SelectionPatch{Route: RouteAuxiliary, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Auxiliary.Enabled == nil || *result.Snapshot.Auxiliary.Enabled {
		t.Fatalf("auxiliary selection = %+v", result.Snapshot.Auxiliary)
	}
	if got := ChangedRoutes(current, result.Snapshot); len(got) != 1 || got[0] != RouteAuxiliary {
		t.Fatalf("changed routes = %v", got)
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
