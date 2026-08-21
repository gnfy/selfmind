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

func roleSetupApp(t *testing.T) (*App, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`models:
  primary:
    provider: deepseek
    model: deepseek-v4-flash
`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &App{
		ctx:         context.Background(),
		stdout:      stdout,
		stderr:      stderr,
		configPath:  configPath,
		interactive: false,
	}, configPath, stdout, stderr
}

func TestEnsureBackgroundRoleSetupUsesPrimaryDefault(t *testing.T) {
	app, configPath, stdout, _ := roleSetupApp(t)
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.Auxiliary.Provider != "" || cfg.Models.Auxiliary.Model != "" {
		t.Fatalf("fixture unexpectedly materialized auxiliary: %+v", cfg.Models.Auxiliary)
	}
	if got := cfg.EffectiveAuxiliary(); got.Provider != "deepseek" || got.Model != "deepseek-v4-flash" {
		t.Fatalf("effective auxiliary = %+v, want the primary default", got)
	}

	app.ensureBackgroundRoleSetup(cfg)

	if got := cfg.Models.Auxiliary; got.Provider != "deepseek" || got.Model != "deepseek-v4-flash" {
		t.Fatalf("initialized auxiliary = %+v, want deepseek/deepseek-v4-flash", got)
	}
	if !strings.Contains(stdout.String(), "Auxiliary model: deepseek/deepseek-v4-flash") {
		t.Errorf("expected a confirmation line, got %q", stdout.String())
	}
}

func TestEnsureBackgroundRoleSetupDoesNotOverwriteExplicitAuxiliary(t *testing.T) {
	app, configPath, stdout, _ := roleSetupApp(t)
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "google", Model: "gemini-flash"}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	app.ensureBackgroundRoleSetup(cfg)

	saved, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Models.Auxiliary; got.Provider != "google" || got.Model != "gemini-flash" {
		t.Fatalf("explicit auxiliary was overwritten: %+v", got)
	}
	if !strings.Contains(stdout.String(), "Auxiliary model: google/gemini-flash") {
		t.Errorf("expected the explicit auxiliary in output, got %q", stdout.String())
	}
}

func TestEnsureBackgroundRoleSetupPreservesRoleOverrides(t *testing.T) {
	app, configPath, _, _ := roleSetupApp(t)
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Models.Roles["memory_extract"] = config.ModelRoleConfig{Provider: "minimax", Model: "MiniMax-M2.7"}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	app.ensureBackgroundRoleSetup(cfg)

	saved, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	kept := saved.Models.Roles["memory_extract"]
	if kept.Provider != "minimax" || kept.Model != "MiniMax-M2.7" {
		t.Fatalf("existing role was overwritten: %+v", kept)
	}
}

func TestEnsureBackgroundRoleSetupNeedsNoInput(t *testing.T) {
	app, configPath, _, stderr := roleSetupApp(t)
	app.interactive = true
	app.stdin = nil
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}

	app.ensureBackgroundRoleSetup(cfg)

	if stderr.Len() != 0 {
		t.Fatalf("automatic auxiliary setup unexpectedly asked for input: %q", stderr.String())
	}
	if cfg.Models.Auxiliary.Provider == "" || cfg.Models.Auxiliary.Model == "" {
		t.Fatal("automatic auxiliary setup did not initialize a route")
	}
}
