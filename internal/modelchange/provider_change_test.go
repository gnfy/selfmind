package modelchange

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestProviderChangesApplyAndRestoreCustomConnection(t *testing.T) {
	cfg := &config.Config{Providers: config.ProvidersConfig{Custom: []config.CustomProvider{{
		Name: "lab", BaseURL: "https://old.example/v1", Protocol: "openai-compatible", Auth: "bearer",
	}}}}
	changes, err := BuildProviderChanges(cfg, []ProviderPatch{{Connection: ProviderConnection{
		ID: "lab", Custom: true, BaseURL: "https://new.example/v1/", Protocol: "responses", Auth: "none",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !ProviderChangesMatch(cfg, changes, false) {
		t.Fatalf("changes = %+v", changes)
	}
	ApplyProviderChanges(cfg, changes, true)
	if !ProviderChangesMatch(cfg, changes, true) {
		t.Fatalf("candidate provider config = %+v", cfg.Providers.Custom)
	}
	provider, _ := cfg.Providers.CustomProvider("lab")
	if provider.BaseURL != "https://new.example/v1" || provider.Protocol != "responses-compatible" || provider.Auth != "none" {
		t.Fatalf("candidate provider = %+v", provider)
	}
	ApplyProviderChanges(cfg, changes, false)
	if !ProviderChangesMatch(cfg, changes, false) {
		t.Fatalf("restored provider config = %+v", cfg.Providers.Custom)
	}
}

func TestProviderChangesRestoreMissingBuiltinOverride(t *testing.T) {
	cfg := &config.Config{}
	changes, err := BuildProviderChanges(cfg, []ProviderPatch{{Connection: ProviderConnection{
		ID: "deepseek", BaseURL: "https://proxy.example/v1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ApplyProviderChanges(cfg, changes, true)
	if !ProviderChangesMatch(cfg, changes, true) {
		t.Fatalf("candidate built-in override = %+v", cfg.Providers.Builtins)
	}
	ApplyProviderChanges(cfg, changes, false)
	if !ProviderChangesMatch(cfg, changes, false) {
		t.Fatalf("missing built-in override was not restored: %+v", cfg.Providers.Builtins)
	}
}

func TestProviderPatchPreservesAdvancedFieldsHiddenByManager(t *testing.T) {
	cfg := &config.Config{Providers: config.ProvidersConfig{Custom: []config.CustomProvider{{
		Name: "lab", BaseURL: "https://old.example/v1", Protocol: "openai-compatible", Auth: "bearer",
		ExtraHeaders: map[string]string{"X-Client": "SelfMind"}, MaxTokens: 8192,
	}}}}
	changes, err := BuildProviderChanges(cfg, []ProviderPatch{{
		Connection:       ProviderConnection{ID: "lab", Custom: true, BaseURL: "https://new.example/v1", Protocol: "responses-compatible", Auth: "none"},
		PreserveAdvanced: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Candidate.ExtraHeaders["X-Client"] != "SelfMind" || changes[0].Candidate.MaxTokens != 8192 {
		t.Fatalf("advanced fields were lost: %+v", changes)
	}
}

func TestProviderPatchRequiresExplicitLegacyUpgrade(t *testing.T) {
	cfg := &config.Config{ProviderProfiles: map[string]config.ProviderEndpoint{
		"deepseek": {BaseURL: "https://legacy.example/v1"},
	}}
	_, err := BuildProviderChanges(cfg, []ProviderPatch{{Connection: ProviderConnection{
		ID: "deepseek", BaseURL: "https://new.example/v1",
	}}})
	if err == nil || !strings.Contains(err.Error(), "selfmind config upgrade") {
		t.Fatalf("error = %v, want explicit upgrade guidance", err)
	}
}

func TestProviderConnectionAndRouteCommitTogether(t *testing.T) {
	service, path := newTestService(t)
	cfg := mustLoadConfig(t, path)
	changes, err := BuildProviderChanges(cfg, []ProviderPatch{{Connection: ProviderConnection{
		ID: "lab", Custom: true, BaseURL: "https://lab.example/v1", Protocol: "openai-compatible", Auth: "none",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := SnapshotFromConfig(cfg)
	candidate.Primary.Provider = "lab"
	candidate.Primary.Model = "lab-model"
	service.Validate = func(_ context.Context, candidateCfg *config.Config, routes []Route) []ProbeResult {
		provider, ok := candidateCfg.Providers.CustomProvider("lab")
		if !ok || provider.BaseURL != "https://lab.example/v1" {
			t.Fatalf("provider change was not visible to validation: %+v", candidateCfg.Providers.Custom)
		}
		return []ProbeResult{{Route: routes[0], OK: true, Provider: "lab", Model: "lab-model"}}
	}
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "test", ProviderChanges: changes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mustLoadConfig(t, path).Providers.CustomProvider("lab"); ok {
		t.Fatal("provider connection was written before the safe boundary")
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	committed := mustLoadConfig(t, path)
	if provider, ok := committed.Providers.CustomProvider("lab"); !ok || provider.BaseURL != "https://lab.example/v1" {
		t.Fatalf("committed provider = %+v, ok=%v", provider, ok)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, rolledBack, err := service.ReconcileStartup(context.Background()); err != nil || rolledBack {
		t.Fatalf("startup rolledBack=%v err=%v", rolledBack, err)
	}
	status, err := service.MarkStartupHealthy()
	if err != nil || status.Running.Primary.Provider != "lab" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
