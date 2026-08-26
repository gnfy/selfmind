package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestSetupPreservesExistingConfigBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`model:
  provider: "kimi-coding"
  default: "kimi-for-coding"

provider_profiles:
  kimi-coding:
    api_key: "${KIMI_CODING_API_KEY}"

custom_unknown_section:
  preserve_me: true
`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "setup", "--non-interactive", "--skip-gateway"},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: configPath,
		onboardingProbe: func(*config.Config, string) error {
			return nil
		},
	}
	handled, code := app.runSetupCommandIfRequested()
	if !handled || code != 0 {
		t.Fatalf("setup = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("setup rewrote an existing config:\n--- want\n%s\n--- got\n%s", original, got)
	}
}

func TestNonInteractiveSetupRejectsMissingModelWithoutStartingGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(),
		args: []string{
			"selfmind",
			"setup",
			"--non-interactive",
			"--skip-gateway",
		},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: configPath,
	}

	handled, code := app.runSetupCommandIfRequested()
	if !handled || code != 1 {
		t.Fatalf("setup = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("No primary model is configured")) {
		t.Fatalf("missing actionable model error: %q", stderr.String())
	}
}

func TestInteractiveSetupCancelDoesNotStartGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	var stdout, stderr bytes.Buffer
	cfg, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		ctx:         context.Background(),
		args:        []string{"selfmind", "setup", "--skip-gateway"},
		stdout:      &stdout,
		stderr:      &stderr,
		configPath:  configPath,
		interactive: true,
	}
	app.stdin = strings.NewReader(fmt.Sprintf("%d\n", len(app.modelChoices(cfg))))

	handled, code := app.runSetupCommandIfRequested()
	if !handled || code != 1 {
		t.Fatalf("setup = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Setup cancelled") {
		t.Fatalf("missing cancellation message: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "Gateway:") {
		t.Fatalf("cancelled setup started the gateway: %q", stdout.String())
	}
}

func TestInitialModelSetupConfiguresModelBeforeTUI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:         context.Background(),
		stdout:      &stdout,
		stderr:      &stderr,
		configPath:  configPath,
		interactive: true,
	}

	got, code := app.ensureInitialModelSetup(cfg, func(cfg *config.Config) int {
		cfg.Providers.OpenAI.APIKey = "sk-test"
		cfg.Providers.OpenAI.Model = "gpt-test"
		cfg.SetDefaultModel("openai", "gpt-test")
		if err := config.SaveConfig(cfg.Path, cfg); err != nil {
			t.Fatal(err)
		}
		return 0
	})
	if code != 0 {
		t.Fatalf("code = %d, stdout:%q stderr:%q", code, stdout.String(), stderr.String())
	}
	if got == nil || got.EffectiveProvider() != "openai" || got.EffectiveModel() != "gpt-test" {
		t.Fatalf("configured model = %#v", got)
	}
	if !strings.Contains(stdout.String(), "Welcome to SelfMind") ||
		!strings.Contains(stdout.String(), "Setup complete: openai/gpt-test") {
		t.Fatalf("missing onboarding output: %q", stdout.String())
	}
}

func TestInitialModelSetupDoesNotPromptNonInteractiveLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:        context.Background(),
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: configPath,
	}
	called := false

	got, code := app.ensureInitialModelSetup(cfg, func(*config.Config) int {
		called = true
		return 0
	})
	if code != 1 || got != nil {
		t.Fatalf("setup = cfg:%#v code:%d", got, code)
	}
	if called {
		t.Fatal("non-interactive launch prompted for model setup")
	}
	if !strings.Contains(stderr.String(), "Run `selfmind setup` or `selfmind model` in an interactive terminal") ||
		!strings.Contains(stderr.String(), "configure `models.primary` in YAML") {
		t.Fatalf("missing non-interactive guidance: %q", stderr.String())
	}
}

func TestInitialModelSetupCancelStopsBeforeTUI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:         context.Background(),
		stdout:      &stdout,
		stderr:      &stderr,
		configPath:  configPath,
		interactive: true,
	}

	got, code := app.ensureInitialModelSetup(cfg, func(*config.Config) int { return 0 })
	if code != 1 || got != nil {
		t.Fatalf("setup = cfg:%#v code:%d", got, code)
	}
	if !strings.Contains(stderr.String(), "Setup cancelled") {
		t.Fatalf("missing cancellation message: %q", stderr.String())
	}
}
