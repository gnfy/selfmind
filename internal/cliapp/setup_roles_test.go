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

func roleSetupApp(t *testing.T, stdin string) (*App, string, *bytes.Buffer, *bytes.Buffer) {
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
		stdin:       strings.NewReader(stdin),
		configPath:  configPath,
		interactive: stdin != "",
	}, configPath, stdout, stderr
}

// Choosing "reuse the foreground model" must persist every missing role, so the
// daemon stops disabling approval triage and memory work on the next start.
func TestEnsureBackgroundRoleSetupReusesForegroundModel(t *testing.T) {
	app, configPath, stdout, _ := roleSetupApp(t, "1\n")
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(missingBackgroundRoles(cfg)) != len(backgroundRoleSpecs) {
		t.Fatalf("fixture should start with every background role missing")
	}

	app.ensureBackgroundRoleSetup(cfg)

	saved, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if missing := missingBackgroundRoles(saved); len(missing) != 0 {
		t.Fatalf("roles still missing after setup: %v", missing)
	}
	for _, spec := range backgroundRoleSpecs {
		role := saved.Models.Roles[string(spec.role)]
		if role.Provider != "deepseek" || role.Model != "deepseek-v4-flash" {
			t.Errorf("role %s = %s/%s, want deepseek/deepseek-v4-flash", spec.role, role.Provider, role.Model)
		}
	}
	if !strings.Contains(stdout.String(), "Background models:") {
		t.Errorf("expected a confirmation line, got %q", stdout.String())
	}
}

// Skipping must leave the config untouched and must not fail setup.
func TestEnsureBackgroundRoleSetupSkipKeepsConfigClean(t *testing.T) {
	app, configPath, _, _ := roleSetupApp(t, "3\n")
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}

	app.ensureBackgroundRoleSetup(cfg)

	saved, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(missingBackgroundRoles(saved)) != len(backgroundRoleSpecs) {
		t.Fatal("skipping must not write any role")
	}
}

// A hand-tuned role must survive: setup only fills the gaps.
func TestEnsureBackgroundRoleSetupOnlyFillsGaps(t *testing.T) {
	app, configPath, _, _ := roleSetupApp(t, "1\n")
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		"memory_extract": {Provider: "minimax", Model: "MiniMax-M2.7"},
	}

	app.ensureBackgroundRoleSetup(cfg)

	saved, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	kept := saved.Models.Roles["memory_extract"]
	if kept.Provider != "minimax" || kept.Model != "MiniMax-M2.7" {
		t.Fatalf("existing role was overwritten: %s/%s", kept.Provider, kept.Model)
	}
	if filled := saved.Models.Roles["fast_classifier"]; filled.Model != "deepseek-v4-flash" {
		t.Fatalf("missing role was not filled: %+v", filled)
	}
}

// Non-interactive runs must explain the fix without prompting or failing.
func TestEnsureBackgroundRoleSetupNonInteractiveGuidance(t *testing.T) {
	app, configPath, _, stderr := roleSetupApp(t, "")
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}

	app.ensureBackgroundRoleSetup(cfg)

	if !strings.Contains(stderr.String(), "models.roles.fast_classifier") {
		t.Errorf("expected actionable guidance, got %q", stderr.String())
	}
	saved, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(missingBackgroundRoles(saved)) != len(backgroundRoleSpecs) {
		t.Fatal("non-interactive run must not write config")
	}
}

// An app marked interactive but with no readable stdin must fall back to
// printed guidance. Prompting there reads from a nil reader and panics.
func TestEnsureBackgroundRoleSetupInteractiveWithoutStdin(t *testing.T) {
	app, configPath, _, stderr := roleSetupApp(t, "")
	app.interactive = true
	app.stdin = nil
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}

	app.ensureBackgroundRoleSetup(cfg)

	if !strings.Contains(stderr.String(), "models.roles.fast_classifier") {
		t.Errorf("expected guidance instead of a prompt, got %q", stderr.String())
	}
}
