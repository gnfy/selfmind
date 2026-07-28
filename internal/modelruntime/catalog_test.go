package modelruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverCodexModelDescriptorFromLocalCache(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	data := []byte(`{
  "models": [{
    "slug": "gpt-5.6-sol",
    "context_window": 272000,
    "effective_context_window_percent": 95,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": [
      {"effort": "low"},
      {"effort": "xhigh"}
    ],
    "default_service_tier": "priority",
    "service_tiers": [{"id": "priority"}],
    "input_modalities": ["text", "image"]
  }]
}`)
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := DiscoverModelDescriptor("codex-cli", "gpt-5.6-sol")
	if !ok {
		t.Fatal("descriptor was not discovered")
	}
	if got.ContextWindow != 272000 || got.DefaultReasoning != "medium" || !got.SupportsVision {
		t.Fatalf("descriptor = %+v", got)
	}
	if len(got.SupportedReasoning) != 2 || got.SupportedReasoning[1] != "xhigh" {
		t.Fatalf("reasoning levels = %v", got.SupportedReasoning)
	}
	if got.DefaultServiceTier != "priority" || len(got.SupportedServiceTiers) != 1 {
		t.Fatalf("service tiers = %q %v", got.DefaultServiceTier, got.SupportedServiceTiers)
	}
}
