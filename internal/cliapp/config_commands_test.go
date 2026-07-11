package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestConfigDoctorReportsMissingDefaultsAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte(`providers:
  openai_api_key: "${OPENAI_API_KEY}"
agent:
  provider: "openai"
  model: "gpt-test"
`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "config", "doctor"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runConfigCommandIfRequested()
	if !handled {
		t.Fatal("config command was not handled")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Config:",
		"Status: upgrade available",
		"Missing defaults:",
		"Migratable legacy keys:",
		"providers.openai_api_key -> providers.openai.api_key",
		"agent.provider -> model.provider",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("doctor modified config:\n%s", string(after))
	}
}

func TestConfigUpgradeBacksUpAndPreservesPlaceholders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "real-secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte(`x_custom:
  keep: true
providers:
  openai_api_key: "${OPENAI_API_KEY}"
agent:
  provider: "openai"
  model: "gpt-test"
intent:
  continue_window: "10m"
`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "config", "upgrade"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runConfigCommandIfRequested()
	if !handled {
		t.Fatal("config command was not handled")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Backup:") {
		t.Fatalf("upgrade output missing backup path:\n%s", out)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	backupData, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupData, original) {
		t.Fatalf("backup did not preserve original:\n%s", string(backupData))
	}

	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(upgraded)
	for _, want := range []string{
		"x_custom:",
		"model:",
		"provider: openai",
		"default: gpt-test",
		"openai:",
		"storage:",
		"gateway:",
		"governance:",
		"mode: shadow",
		"pause_while_run_active: true",
		"${OPENAI_API_KEY}",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("upgraded config missing %q:\n%s", want, text)
		}
	}
	for _, unexpected := range []string{"real-secret", "openai_api_key", "continue_window"} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("upgraded config contains %q:\n%s", unexpected, text)
		}
	}
	if _, err := config.LoadConfig(config.Options{Path: path}); err != nil {
		t.Fatalf("upgraded config is not loadable: %v\n%s", err, text)
	}
}

func TestConfigUpgradeAddsKimiCheapRoleDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte(`model:
  provider: "codex-cli"
  default: "gpt-5.5"
provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"
models:
  roles:
    background_review:
      provider: "custom"
      model: "keep-me"
`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}

	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "config", "upgrade"},
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		configPath: path,
	}

	handled, code := app.runConfigCommandIfRequested()
	if !handled || code != 0 {
		t.Fatalf("upgrade handled=%v code=%d stderr=%s", handled, code, app.stderr)
	}
	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(upgraded)
	for _, role := range []string{"fast_classifier", "memory_extract", "skill_curator", "semantic_recall"} {
		if !strings.Contains(text, role+":") {
			t.Fatalf("upgraded config missing role %q:\n%s", role, text)
		}
	}
	if !strings.Contains(text, `provider: "custom"`) || !strings.Contains(text, `model: "keep-me"`) {
		t.Fatalf("existing role was overwritten:\n%s", text)
	}
	if !strings.Contains(text, `provider: kimi-coding`) || !strings.Contains(text, `model: kimi-for-coding`) {
		t.Fatalf("kimi role defaults missing:\n%s", text)
	}
	if _, err := config.LoadConfig(config.Options{Path: path}); err != nil {
		t.Fatalf("upgraded config is not loadable: %v\n%s", err, text)
	}
}

func TestConfigDiagnosticsSectionReportsModelAndLegacyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  openai_api_key: "${OPENAI_API_KEY}"
agent:
  provider: "openai"
  model: "gpt-test"
`), 0600); err != nil {
		t.Fatal(err)
	}

	app := &App{ctx: context.Background(), configPath: path}
	diag := app.collectConfigDiagnostics()
	section := diag.section()

	for _, want := range []string{
		"== Config ==",
		"status: upgrade available",
		"providers.openai_api_key -> providers.openai.api_key",
		"model: not ready",
		"model_check: selfmind model check",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("diagnostic section missing %q:\n%s", want, section)
		}
	}
	warnings := strings.Join(diag.startupWarnings(), "\n")
	for _, want := range []string{"config can be upgraded", "AI model is not ready"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("startup warnings missing %q: %q", want, warnings)
		}
	}
}

func TestConfigDiagnosticsWarnsForMissingDefaultsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`model:
  provider: "codex-cli"
  default: "gpt-5.5"
`), 0600); err != nil {
		t.Fatal(err)
	}

	app := &App{ctx: context.Background(), configPath: path}
	warnings := strings.Join(app.collectConfigDiagnostics().startupWarnings(), "\n")
	if !strings.Contains(warnings, "config can be upgraded") {
		t.Fatalf("startup warnings should mention upgradeable config: %q", warnings)
	}
}
