package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
