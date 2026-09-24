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

// A probe that proves how a provider turns reasoning off must leave that
// encoding in config.yaml once the change commits, so every later approval
// request uses it without the person having to know the setting exists. A
// thinking_mode the person declared is never replaced.
func TestConfirmedChangeRecordsProbedThinkingMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		declared  string
		want      string
		wantSaved bool
	}{
		{name: "undeclared provider", want: "effort_none", wantSaved: true},
		{name: "declared provider keeps its mode", declared: "omit", want: "omit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, path := newTestService(t)
			cfg := mustLoadConfig(t, path)
			cfg.Providers.Custom = append(cfg.Providers.Custom, config.CustomProvider{
				Name: "lab", BaseURL: "https://lab.example/v1", Protocol: "openai-compatible", Auth: "bearer",
				Quirks: config.ProviderQuirks{ThinkingMode: tc.declared},
			})
			if err := config.SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			service.Validate = func(_ context.Context, _ *config.Config, routes []Route) []ProbeResult {
				results := make([]ProbeResult, 0, len(routes))
				for _, route := range routes {
					results = append(results, ProbeResult{
						Route: route, OK: true, Provider: "lab", Model: "lab-model",
						ThinkingMode: "effort_none", Notice: "The approval model kept reasoning when asked not to.",
					})
				}
				return results
			}
			status, err := service.Inspect()
			if err != nil {
				t.Fatal(err)
			}
			candidate := status.Configured
			candidate.Auxiliary = config.ModelSelectionConfig{Provider: "lab", Model: "lab-model"}
			prepared, err := service.Prepare(context.Background(), PrepareRequest{Candidate: candidate, Source: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
				t.Fatal(err)
			}
			provider, ok := mustLoadConfig(t, path).Providers.CustomProvider("lab")
			if !ok || provider.Quirks.ThinkingMode != tc.want {
				t.Fatalf("committed thinking_mode = %q, want %q", provider.Quirks.ThinkingMode, tc.want)
			}
			notices := strings.Join(ProbeNotices(prepared.Change.Probes), " ")
			if saved := strings.Contains(notices, "Saved quirks.thinking_mode: effort_none for provider lab."); saved != tc.wantSaved {
				t.Fatalf("notices %q: saved=%t, want %t", notices, saved, tc.wantSaved)
			}
			if kept := strings.Contains(notices, "declares thinking_mode: omit, so it was left unchanged"); kept == tc.wantSaved {
				t.Fatalf("notices %q must say why a declared mode was kept", notices)
			}
		})
	}
}
