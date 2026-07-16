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

func TestSaveConfigWritesNewProviderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{
		Path: path,
		Model: ModelConfig{
			Provider: "openai",
			Default:  "gpt-test",
		},
		Providers: ProvidersConfig{
			OpenAI: ProviderEndpoint{APIKey: "${OPENAI_API_KEY}", BaseURL: "https://api.openai.com/v1"},
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
	if !strings.Contains(text, "model:") || !strings.Contains(text, "providers:") || !strings.Contains(text, "openai:") {
		t.Fatalf("saved config missing new schema sections:\n%s", text)
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
	if !cfg.Tasks.InboxEnabled || cfg.Tasks.ListLimit() != 10 || cfg.Tasks.MaintenanceModelRole != "memory_extract" || len(cfg.Tasks.MaintenanceFallbackRoles) != 2 {
		t.Fatalf("task defaults = %+v", cfg.Tasks)
	}
	if doneAfter != 30*24*time.Hour || cancelledAfter != 7*24*time.Hour {
		t.Fatalf("archive defaults = %s/%s", doneAfter, cancelledAfter)
	}
	if debounce != 5*time.Minute || maxWait != 15*time.Minute || batchMax != 10 {
		t.Fatalf("maintenance defaults = %s/%s/%d", debounce, maxWait, batchMax)
	}

	overridePath := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(overridePath, []byte(`
tasks:
  inbox_enabled: false
  default_list_limit: 99
  auto_archive_done_after: "48h"
  auto_archive_cancelled_after: "0"
  maintenance_model_role: "fast_classifier"
  maintenance_fallback_roles: ["background_review"]
  maintenance_debounce: "2m"
  maintenance_max_wait: "10m"
  maintenance_batch_max_runs: 99
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(Options{Path: overridePath})
	if err != nil {
		t.Fatal(err)
	}
	doneAfter, cancelledAfter = cfg.Tasks.AutoArchiveDurations()
	debounce, maxWait, batchMax = cfg.Tasks.MaintenanceBatchPolicy()
	if cfg.Tasks.InboxEnabled || cfg.Tasks.ListLimit() != 50 || cfg.Tasks.MaintenanceModelRole != "fast_classifier" || len(cfg.Tasks.MaintenanceFallbackRoles) != 1 || cfg.Tasks.MaintenanceFallbackRoles[0] != "background_review" {
		t.Fatalf("task overrides = %+v", cfg.Tasks)
	}
	if doneAfter != 48*time.Hour || cancelledAfter != 0 {
		t.Fatalf("archive overrides = %s/%s", doneAfter, cancelledAfter)
	}
	if debounce != 2*time.Minute || maxWait != 10*time.Minute || batchMax != 50 {
		t.Fatalf("maintenance overrides = %s/%s/%d", debounce, maxWait, batchMax)
	}
}
