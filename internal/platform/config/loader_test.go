package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigSupportsExplicitPathAndLegacyModelFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
agent:
  provider: "anthropic"
  model: "claude-test"
providers:
  anthropic_api_key: "legacy-key"
storage:
  data_dir: "./data"
`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.EffectiveProvider(), "anthropic"; got != want {
		t.Fatalf("provider = %q, want %q", got, want)
	}
	if got, want := cfg.EffectiveModel(), "claude-test"; got != want {
		t.Fatalf("model = %q, want %q", got, want)
	}
	if got, want := cfg.Providers.Anthropic.APIKey, "legacy-key"; got != want {
		t.Fatalf("anthropic key = %q, want %q", got, want)
	}
}

func TestTUIThemeDefaultsNormalizesAndValidates(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TUI.Theme != "auto" {
			t.Fatalf("tui.theme = %q, want auto", cfg.TUI.Theme)
		}
	})

	t.Run("explicit mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("tui:\n  theme: LIGHT\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TUI.Theme != "light" {
			t.Fatalf("tui.theme = %q, want light", cfg.TUI.Theme)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("tui:\n  theme: solarized\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(Options{Path: path}); err == nil || !strings.Contains(err.Error(), "invalid tui.theme") {
			t.Fatalf("LoadConfig error = %v, want invalid tui.theme", err)
		}
	})
}

func TestProviderExtraOptionsNormalizeEnvironmentAndPreserveLegacyHeaders(t *testing.T) {
	t.Setenv("PROVIDER_USER", "team-user-42")
	t.Setenv("PROVIDER_TOKEN", "opaque-token-42")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
provider_profiles:
  deepseek:
    headers:
      X-Legacy: legacy
    extra_headers:
      X-Token: ${PROVIDER_TOKEN}
    extra_body:
      user_id: ${PROVIDER_USER}
      nested:
        enabled: true
    extra_query:
      api-version: "2026-08-11"
models:
  auxiliary:
    provider: deepseek
    model: deepseek-v4-flash
  roles:
    memory_extract:
      extra_body:
        nested:
          mode: compact
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.ProviderProfiles["deepseek"]
	if profile.Headers["x-legacy"] != "legacy" || profile.ExtraHeaders["x-token"] != "opaque-token-42" {
		t.Fatalf("headers = legacy:%q extra:%q profiles=%#v", profile.Headers["x-legacy"], profile.ExtraHeaders["x-token"], cfg.ProviderProfiles)
	}
	if profile.ExtraBody["user_id"] != "team-user-42" || profile.ExtraQuery["api-version"] != "2026-08-11" {
		t.Fatalf("extra options = body:%#v query:%#v", profile.ExtraBody, profile.ExtraQuery)
	}
	role, _, ok := cfg.ResolveAuxiliaryRole("memory_extract")
	if !ok || role.ExtraBody["nested"].(map[string]interface{})["mode"] != "compact" {
		t.Fatalf("role extra_body = %#v", role.ExtraBody)
	}
}

func TestProviderQuirkBooleansPreserveExplicitFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
provider_profiles:
  anthropic:
    quirks:
      prompt_cache: false
      responses_store_false: true
      responses_require_stream: false
      http_version: http2
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	quirks := cfg.ProviderProfiles["anthropic"].Quirks
	if quirks.PromptCache == nil || *quirks.PromptCache {
		t.Fatalf("prompt_cache = %v, want explicit false", quirks.PromptCache)
	}
	if quirks.ResponsesStoreFalse == nil || !*quirks.ResponsesStoreFalse {
		t.Fatalf("responses_store_false = %v, want explicit true", quirks.ResponsesStoreFalse)
	}
	if quirks.ResponsesRequireStream == nil || *quirks.ResponsesRequireStream {
		t.Fatalf("responses_require_stream = %v, want explicit false", quirks.ResponsesRequireStream)
	}
	if quirks.HTTPVersion != "http2" {
		t.Fatalf("http_version = %q, want http2", quirks.HTTPVersion)
	}
}

func TestModelsPrimaryOverridesLegacySelectionAndNormalizesAuto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
model:
  provider: "legacy"
  default: "legacy-model"
models:
  primary:
    provider: "codex-cli"
    model: "gpt-5.6-sol"
    reasoning: "auto"
`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.EffectiveProvider(), "codex-cli"; got != want {
		t.Fatalf("provider = %q, want %q", got, want)
	}
	if got, want := cfg.EffectiveModel(), "gpt-5.6-sol"; got != want {
		t.Fatalf("model = %q, want %q", got, want)
	}
	if got := cfg.EffectivePrimary().Reasoning; got != "" {
		t.Fatalf("reasoning = %q, want provider default", got)
	}
}

func TestSetPrimaryModelConvergesAwayFromLegacySelection(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{Provider: "legacy", Default: "legacy-model"},
		Agent: AgentConfig{Provider: "legacy", Model: "legacy-model"},
	}
	cfg.SetPrimaryModel("codex-cli", "gpt-5.6-sol", "xhigh")

	if got := cfg.EffectivePrimary(); got.Provider != "codex-cli" || got.Model != "gpt-5.6-sol" || got.Reasoning != "xhigh" {
		t.Fatalf("primary = %+v", got)
	}
	if cfg.Model.Provider != "" || cfg.Model.Default != "" || cfg.Agent.Provider != "" || cfg.Agent.Model != "" {
		t.Fatalf("legacy selection fields were not cleared: model=%+v agent=%+v", cfg.Model, cfg.Agent)
	}
	if got := cfg.Models.Auxiliary; got.Provider != "codex-cli" || got.Model != "gpt-5.6-sol" {
		t.Fatalf("initial auxiliary = %+v, want the primary provider/model", got)
	}
}

func TestAuxiliaryModelRoutesBackgroundRolesWithExplicitOverride(t *testing.T) {
	cfg := &Config{Models: ModelsConfig{
		Primary:   ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-primary"},
		Auxiliary: ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-v4-flash", Reasoning: "low"},
		Roles: map[string]ModelRoleConfig{
			"memory_extract": {Model: "deepseek-memory", MaxTokens: 2048},
		},
	}}
	cfg.Normalize()

	fast, source, ok := cfg.ResolveAuxiliaryRole("fast_classifier")
	if !ok || source != "auxiliary" || fast.Provider != "deepseek" || fast.Model != "deepseek-v4-flash" {
		t.Fatalf("fast classifier route = %+v source=%q ok=%v", fast, source, ok)
	}
	memory, source, ok := cfg.ResolveAuxiliaryRole("memory_extract")
	if !ok || source != "role" || memory.Provider != "deepseek" || memory.Model != "deepseek-memory" || memory.MaxTokens != 2048 {
		t.Fatalf("memory route = %+v source=%q ok=%v", memory, source, ok)
	}
	if _, _, ok := cfg.ResolveAuxiliaryRole("vision"); !ok {
		// ResolveAuxiliaryRole is intentionally generic; callers decide which
		// roles may inherit auxiliary. This assertion documents only that the
		// config layer can provide a base when requested.
		t.Fatal("expected generic auxiliary resolution")
	}
}

func TestMissingAuxiliaryDefaultsToPrimary(t *testing.T) {
	cfg := &Config{Models: ModelsConfig{Primary: ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-primary"}}}
	cfg.Normalize()
	if got := cfg.EffectiveAuxiliary(); got.Provider != "codex-cli" || got.Model != "gpt-primary" {
		t.Fatalf("auxiliary default = %+v, want primary", got)
	}
	if got, source, ok := cfg.ResolveAuxiliaryRole("memory_extract"); !ok || source != "auxiliary" || got.Provider != "codex-cli" || got.Model != "gpt-primary" {
		t.Fatalf("memory_extract route = %+v source=%q ok=%t, want auxiliary default", got, source, ok)
	}
}

func TestPrimaryChangesDoNotOverwriteMaterializedAuxiliary(t *testing.T) {
	cfg := &Config{}
	cfg.SetPrimaryModel("codex-cli", "gpt-first", "xhigh")
	cfg.SetPrimaryModel("anthropic", "claude-next", "high")

	if got := cfg.EffectivePrimary(); got.Provider != "anthropic" || got.Model != "claude-next" {
		t.Fatalf("primary = %+v", got)
	}
	if got := cfg.Models.Auxiliary; got.Provider != "codex-cli" || got.Model != "gpt-first" {
		t.Fatalf("auxiliary was overwritten by a later primary change: %+v", got)
	}
}

func TestExecSandboxNetworkDefaultAndExplicitOverride(t *testing.T) {
	defaultPath := filepath.Join(t.TempDir(), "default.yaml")
	cfg, err := LoadConfig(Options{Path: defaultPath, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ExecSandbox.AllowNetwork {
		t.Fatal("exec sandbox network must default to shared")
	}

	overridePath := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(overridePath, []byte("exec_sandbox:\n  allow_network: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(Options{Path: overridePath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecSandbox.AllowNetwork {
		t.Fatal("explicit allow_network=false must be preserved")
	}
}

func TestUpdateCheckDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfig(Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Updates.Enabled || cfg.Updates.Channel != "auto" || cfg.Updates.CheckInterval != "15m" {
		t.Fatalf("update defaults = %+v, want enabled/auto/15m", cfg.Updates)
	}
}

func TestEvolutionDefaultsToObservationOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfig(Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Evolution.Enabled || cfg.Evolution.Mode != "observe" {
		t.Fatalf("evolution defaults = enabled:%t mode:%q, want true/observe", cfg.Evolution.Enabled, cfg.Evolution.Mode)
	}
}

func TestAutomaticRunRecoveryDefaultsEnabledAndCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadConfig(Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Gateway.AutomaticRunRecovery {
		t.Fatal("automatic run recovery should default enabled")
	}
	if err := os.WriteFile(path, []byte("gateway:\n  automatic_run_recovery: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.AutomaticRunRecovery {
		t.Fatal("explicit automatic_run_recovery=false was not preserved")
	}
}

func TestSaveConfigWritesNewProviderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{
		Path: path,
		Models: ModelsConfig{
			Primary: ModelSelectionConfig{
				Provider: "openai",
				Model:    "gpt-test",
			},
		},
		Providers: ProvidersConfig{
			OpenAI: ProviderEndpoint{APIKey: "${OPENAI_API_KEY}", BaseURL: "https://proxy.example/v1"},
		},
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, unexpected := range []string{"openai_api_key", "anthropic_api_key", "gemini_api_key"} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("saved config should not contain legacy key %q:\n%s", unexpected, text)
		}
	}
	if !strings.Contains(text, "models:") || !strings.Contains(text, "primary:") || !strings.Contains(text, "providers:") || !strings.Contains(text, "openai:") {
		t.Fatalf("saved config missing new schema sections:\n%s", text)
	}
}

func TestLoadConfigSupportsBuiltinOverridesAndMapCustomProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  deepseek:
    base_url: "https://proxy.example/v1"
  custom:
    deepseek-local:
      base_url: "http://127.0.0.1:8000/v1"
      protocol: openai-compatible
      auth: none
      extra_headers:
        X-Application: SelfMind
models:
  primary:
    provider: deepseek
    model: deepseek-chat
  auxiliary:
    enabled: false
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, ok := cfg.Providers.BuiltinEndpoint("deepseek")
	if !ok || endpoint.BaseURL != "https://proxy.example/v1" {
		t.Fatalf("deepseek endpoint = %#v, ok=%t", endpoint, ok)
	}
	custom, ok := cfg.Providers.CustomProvider("deepseek-local")
	if !ok || custom.Protocol != "openai-compatible" || custom.Auth != "none" || custom.ExtraHeaders["x-application"] != "SelfMind" {
		t.Fatalf("custom provider = %#v, ok=%t", custom, ok)
	}
	if cfg.Models.Auxiliary.Enabled == nil || *cfg.Models.Auxiliary.Enabled {
		t.Fatalf("auxiliary enabled = %v, want explicit false", cfg.Models.Auxiliary.Enabled)
	}
	if cfg.AuxiliaryEnabled() {
		t.Fatal("explicitly disabled auxiliary route reported enabled")
	}
	if _, _, ok := cfg.ResolveAuxiliaryRole("memory_extract"); ok {
		t.Fatal("disabled background route still resolved a maintenance model")
	}
}

func TestSaveConfigWritesCustomProvidersAsMapAndOmitsBuiltinDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{
		Path:   path,
		Models: ModelsConfig{Primary: ModelSelectionConfig{Provider: "openai", Model: "gpt-test"}},
		Providers: ProvidersConfig{Custom: []CustomProvider{{
			Name: "company-gateway", BaseURL: "https://llm.company.example/v1", Protocol: "openai-compatible",
		}}},
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "- name: company-gateway") || !strings.Contains(text, "company-gateway:") {
		t.Fatalf("custom providers must use map form:\n%s", text)
	}
	if strings.Contains(text, "https://api.openai.com/v1") || strings.Contains(text, "https://api.anthropic.com") {
		t.Fatalf("built-in defaults must not be materialized:\n%s", text)
	}
}

func TestLoadConfigRejectsUnknownCustomProviderField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom:
    local:
      baseurl: "http://127.0.0.1:8000/v1"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(Options{Path: path})
	if err == nil || !strings.Contains(err.Error(), "providers.custom.local.baseurl") || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("error = %v, want path and suggestion", err)
	}
}

func TestLoadConfigRejectsUnknownTopLevelProviderID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  opneai:
    base_url: https://proxy.example/v1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(Options{Path: path})
	if err == nil || !strings.Contains(err.Error(), "did you mean providers.openai") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsCaseInsensitiveDuplicateProviderHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom:
    router:
      base_url: "https://router.example/v1"
      extra_headers:
        X-Title: One
        x-title: Two
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(Options{Path: path})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "duplicate header") || !strings.Contains(strings.ToLower(err.Error()), "x-title") {
		t.Fatalf("error = %v, want duplicate header failure", err)
	}
}

func TestLoadConfigRejectsCredentialBearingProviderHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  custom:
    lab:
      base_url: https://lab.example/v1
      extra_headers:
        Authorization: Bearer embedded-secret
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(Options{Path: path})
	if err == nil || !strings.Contains(err.Error(), "credential-bearing") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsCredentialBearingProviderExtras(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  custom:
    lab:
      base_url: https://lab.example/v1
      extra_body:
        metadata:
          access_token: embedded-secret
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(Options{Path: path})
	if err == nil || !strings.Contains(err.Error(), "extra_body.metadata.access_token") || !strings.Contains(err.Error(), "credential-bearing") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsAmbiguousCustomProviderIDs(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "built-in collision",
			yaml: "providers:\n  custom:\n    openai:\n      base_url: https://proxy.example/v1\n",
			want: "collides with a built-in provider",
		},
		{
			name: "normalized duplicate",
			yaml: "providers:\n  custom:\n    company_api:\n      base_url: https://one.example/v1\n    company-api:\n      base_url: https://two.example/v1\n",
			want: "duplicates",
		},
		{
			name: "non-mapping providers",
			yaml: "providers: []\n",
			want: "providers must be a mapping",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(Options{Path: path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResolveConfigPathPrefersEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	t.Setenv("SELF_CONFIG", path)
	got, isDefault := ResolveConfigPath("")
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	if isDefault {
		t.Fatal("SELF_CONFIG path should not be treated as default")
	}
}

func TestWeixinConfigReadsHermesStyleEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("WEIXIN_ACCOUNT_ID", "wx-account")
	t.Setenv("WEIXIN_TOKEN", "wx-token")
	t.Setenv("WEIXIN_BASE_URL", "https://example.weixin")
	t.Setenv("WEIXIN_CDN_BASE_URL", "https://cdn.weixin")
	t.Setenv("WEIXIN_DM_POLICY", "allowlist")
	t.Setenv("WEIXIN_ALLOWED_USERS", "alice,bob")
	t.Setenv("WEIXIN_SPLIT_MULTILINE_MESSAGES", "true")

	cfg, err := LoadConfig(Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	wx := cfg.Gateway.Weixin
	if wx.AccountID != "wx-account" || wx.Token != "wx-token" {
		t.Fatalf("weixin credentials = %+v", wx)
	}
	if wx.BaseURL != "https://example.weixin" || wx.CDNBaseURL != "https://cdn.weixin" {
		t.Fatalf("weixin URLs = %+v", wx)
	}
	if wx.DMPolicy != "allowlist" || len(wx.AllowFrom) != 2 || wx.AllowFrom[0] != "alice" || !wx.SplitMultilineMessages {
		t.Fatalf("weixin policy = %+v", wx)
	}
}

func TestTaskGovernanceDefaultsAndOverrides(t *testing.T) {
	defaultPath := filepath.Join(t.TempDir(), "default.yaml")
	cfg, err := LoadConfig(Options{Path: defaultPath, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	doneAfter, cancelledAfter := cfg.Tasks.AutoArchiveDurations()
	debounce, maxWait, batchMax := cfg.Tasks.MaintenanceBatchPolicy()
	probeInitial, probeMax := cfg.Tasks.MaintenanceQuotaCircuitPolicy()
	softProbeInitial, softProbeMax := cfg.Tasks.MaintenanceSoftCircuitPolicy()
	// A fresh config carries no legacy fallback roles: background maintenance
	// falls back to models.auxiliary, so only an old config can carry the key
	// until config upgrade removes it.
	if !cfg.Tasks.InboxEnabled || cfg.Tasks.ListLimit() != 10 || len(cfg.Tasks.MaintenanceFallbackRoles) != 0 {
		t.Fatalf("task defaults = %+v", cfg.Tasks)
	}
	if doneAfter != 30*24*time.Hour || cancelledAfter != 7*24*time.Hour {
		t.Fatalf("archive defaults = %s/%s", doneAfter, cancelledAfter)
	}
	if debounce != 5*time.Minute || maxWait != 15*time.Minute || batchMax != 10 {
		t.Fatalf("maintenance defaults = %s/%s/%d", debounce, maxWait, batchMax)
	}
	if probeInitial != 15*time.Minute || probeMax != 4*time.Hour {
		t.Fatalf("quota circuit defaults = %s/%s", probeInitial, probeMax)
	}
	if softProbeInitial != 15*time.Minute || softProbeMax != time.Hour {
		t.Fatalf("soft circuit defaults = %s/%s", softProbeInitial, softProbeMax)
	}

	overridePath := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(overridePath, []byte(`
tasks:
  inbox_enabled: false
  default_list_limit: 99
  auto_archive_done_after: "48h"
  auto_archive_cancelled_after: "0"
  maintenance_fallback_roles: ["background_review"]
  maintenance_debounce: "2m"
  maintenance_max_wait: "10m"
  maintenance_batch_max_runs: 99
  maintenance_quota_probe_initial: "3m"
  maintenance_quota_probe_max: "30m"
  maintenance_soft_probe_initial: "4m"
  maintenance_soft_probe_max: "20m"
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(Options{Path: overridePath})
	if err != nil {
		t.Fatal(err)
	}
	doneAfter, cancelledAfter = cfg.Tasks.AutoArchiveDurations()
	debounce, maxWait, batchMax = cfg.Tasks.MaintenanceBatchPolicy()
	probeInitial, probeMax = cfg.Tasks.MaintenanceQuotaCircuitPolicy()
	softProbeInitial, softProbeMax = cfg.Tasks.MaintenanceSoftCircuitPolicy()
	if cfg.Tasks.InboxEnabled || cfg.Tasks.ListLimit() != 50 || len(cfg.Tasks.MaintenanceFallbackRoles) != 1 || cfg.Tasks.MaintenanceFallbackRoles[0] != "background_review" {
		t.Fatalf("task overrides = %+v", cfg.Tasks)
	}
	if doneAfter != 48*time.Hour || cancelledAfter != 0 {
		t.Fatalf("archive overrides = %s/%s", doneAfter, cancelledAfter)
	}
	if debounce != 2*time.Minute || maxWait != 10*time.Minute || batchMax != 50 {
		t.Fatalf("maintenance overrides = %s/%s/%d", debounce, maxWait, batchMax)
	}
	if probeInitial != 3*time.Minute || probeMax != 30*time.Minute {
		t.Fatalf("quota circuit overrides = %s/%s", probeInitial, probeMax)
	}
	if softProbeInitial != 4*time.Minute || softProbeMax != 20*time.Minute {
		t.Fatalf("soft circuit overrides = %s/%s", softProbeInitial, softProbeMax)
	}
}
