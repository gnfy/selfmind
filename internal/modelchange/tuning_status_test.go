package modelchange

import (
	"testing"

	"selfmind/internal/platform/config"
)

func TestTuningSnapshotSeparatesAutoDefaultFromExplicitReasoning(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-flash"},
			Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-flash", Reasoning: "xhigh"},
		},
		ProviderProfiles: map[string]config.ProviderEndpoint{"deepseek": {APIKey: "test-key"}},
	}
	cfg.Normalize()

	got := tuningSnapshot(cfg, SnapshotFromConfig(cfg))
	if got.Primary.Reasoning != "high" || got.Primary.ReasoningSource != "model_default" {
		t.Fatalf("primary tuning = %+v", got.Primary)
	}
	if got.Auxiliary.Reasoning != "xhigh" || got.Auxiliary.ReasoningSource != "explicit" {
		t.Fatalf("auxiliary tuning = %+v", got.Auxiliary)
	}
}

func TestTuningSnapshotReportsProviderOverrideWithoutChangingUserSelection(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-flash"}},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"deepseek": {APIKey: "test-key", ReasoningEffort: "xhigh"},
		},
	}
	cfg.Normalize()
	snapshot := SnapshotFromConfig(cfg)
	if snapshot.Primary.Reasoning != "" {
		t.Fatalf("user selection reasoning = %q", snapshot.Primary.Reasoning)
	}
	got := tuningSnapshot(cfg, snapshot)
	if got.Primary.Reasoning != "xhigh" || got.Primary.ReasoningSource != "provider_override" {
		t.Fatalf("primary tuning = %+v", got.Primary)
	}
}
