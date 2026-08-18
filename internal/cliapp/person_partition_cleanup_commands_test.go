package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestCleanupPersonPartitionsUsesConfiguredRootsWithoutCreatingStore(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "control-data")
	skillsDir := filepath.Join(root, "custom-skills-root")
	configPath := filepath.Join(root, "config.yaml")
	configBody := fmt.Sprintf("storage:\n  data_dir: %q\nevolution:\n  skills_dir: %q\n", dataDir, skillsDir)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	app := &App{ctx: context.Background(), stdout: out, stderr: out, configPath: configPath}
	if code := app.runMaintenanceCleanupPersonPartitions(nil); code == 0 {
		t.Fatalf("missing control store was accepted:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "control.db")); !os.IsNotExist(err) {
		t.Fatalf("cleanup created control.db: %v", err)
	}

	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := app.runMaintenanceCleanupPersonPartitions(nil); code != 0 {
		t.Fatalf("dry run failed: code=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "Root: "+skillsDir) || !strings.Contains(out.String(), "Control DB: "+filepath.Join(dataDir, "control.db")) {
		t.Fatalf("configured roots not reported:\n%s", out.String())
	}
}

func TestCleanupPersonPartitionsRejectsBrokenConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	app := &App{ctx: context.Background(), stdout: out, stderr: out, configPath: configPath}
	if code := app.runMaintenanceCleanupPersonPartitions(nil); code == 0 || !strings.Contains(out.String(), "load config") {
		t.Fatalf("broken config result: code=%d output=%s", code, out.String())
	}
}

func TestCleanupPersonPartitionsRequiresExplicitDatabaseForRootOverride(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "control-data")
	configuredSkills := filepath.Join(root, "configured-skills")
	overrideSkills := filepath.Join(root, "old-skills")
	configPath := filepath.Join(root, "config.yaml")
	configBody := fmt.Sprintf("storage:\n  data_dir: %q\nevolution:\n  skills_dir: %q\n", dataDir, configuredSkills)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(overrideSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	app := &App{ctx: context.Background(), stdout: out, stderr: out, configPath: configPath}
	if code := app.runMaintenanceCleanupPersonPartitions([]string{"--root", overrideSkills}); code == 0 || !strings.Contains(out.String(), "pass --data-dir explicitly") {
		t.Fatalf("unpaired override result: code=%d output=%s", code, out.String())
	}
	out.Reset()
	if code := app.runMaintenanceCleanupPersonPartitions([]string{"--root", overrideSkills, "--data-dir", dataDir}); code != 0 {
		t.Fatalf("explicitly paired override failed: code=%d output=%s", code, out.String())
	}
}
