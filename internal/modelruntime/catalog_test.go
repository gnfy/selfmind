package modelruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCatalogReportsStaleCacheInsteadOfPresentingItAsLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_cache.json")
	profile := ProviderProfile{ID: "openai", ModelList: ModelListOpenAICompatible, FallbackModels: []string{"fallback"}}
	runtime := Runtime{BaseURL: "http://127.0.0.1:1/v1", APIKey: "secret"}
	key := profile.ID + "|" + runtime.BaseURL + "|" + credentialFingerprint(runtime.APIKey)
	data := []byte(fmt.Sprintf(`{%q:{"ts":%d,"models":["cached-new"]}}`, key, time.Now().Add(-2*time.Hour).Unix()))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := NewCatalog(path).ModelsWithStatus(ctx, profile, runtime, false)
	if len(result.Models) != 1 || result.Models[0] != "cached-new" || result.Source != "cache" || !result.Stale {
		t.Fatalf("result = %+v", result)
	}
	if result.FetchError == nil {
		t.Fatal("stale fallback did not retain the refresh failure")
	}
}
