package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/modelchange"
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
	if _, err := (&modelchange.Service{ConfigPath: configPath}).AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "setup", "--non-interactive", "--skip-gateway"},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: configPath,
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
	if !bytes.Contains(stderr.Bytes(), []byte("Run `selfmind model`")) {
		t.Fatalf("missing actionable model error: %q", stderr.String())
	}
}

func TestExplicitSetupRoutesMissingReadinessToSoleModelCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	var stdout, stderr bytes.Buffer
	_, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true})
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
	handled, code := app.runSetupCommandIfRequested()
	if !handled || code != 1 {
		t.Fatalf("setup = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Run `selfmind model`") {
		t.Fatalf("missing sole Model Manager guidance: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "Gateway:") {
		t.Fatalf("cancelled setup started the gateway: %q", stdout.String())
	}
}
