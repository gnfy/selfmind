package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
	"selfmind/internal/promptassets"
)

func resolveAndActivatePromptSnapshot(cfg *config.Config, dataDir string) (*promptassets.Snapshot, PromptSnapshotStatus) {
	snapshot, status := InspectRuntimePromptSnapshot(cfg, dataDir)
	return snapshot, ActivateRuntimePromptSnapshot(snapshot, status, dataDir)
}

func TestResolveRuntimePromptSnapshotFallsBackToLastKnownGood(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	cfg := &config.Config{Path: filepath.Join(root, "config.yaml")}
	promptRoot := filepath.Join(root, "prompts")
	if err := os.MkdirAll(promptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(promptRoot, "agent.md")
	if err := os.WriteFile(agentPath, []byte("## Persona\n\nKeep this known-good guidance.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	active, activeStatus := resolveAndActivatePromptSnapshot(cfg, dataDir)
	if activeStatus.Degraded() || activeStatus.Source != PromptSourceActive {
		t.Fatalf("active status = %+v", activeStatus)
	}
	if got := active.Custom(promptassets.FileAgent, "Persona"); got != "Keep this known-good guidance." {
		t.Fatalf("active persona = %q", got)
	}
	activationPath := promptActivationPath(dataDir)
	activationInfo, err := os.Stat(activationPath)
	if err != nil {
		t.Fatal(err)
	}
	if activationInfo.Mode().Perm() != 0o600 {
		t.Fatalf("activation mode = %o", activationInfo.Mode().Perm())
	}
	activationData, err := os.ReadFile(activationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(activationData) == "" || strings.Contains(string(activationData), "Keep this known-good guidance.") {
		t.Fatalf("activation record leaked prompt content: %s", activationData)
	}

	if err := os.WriteFile(agentPath, []byte("## Unknown\n\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback, fallbackStatus := resolveAndActivatePromptSnapshot(cfg, dataDir)
	if !fallbackStatus.Degraded() || fallbackStatus.Source != PromptSourceLastKnownGood {
		t.Fatalf("fallback status = %+v", fallbackStatus)
	}
	if fallback.Hash() != active.Hash() {
		t.Fatalf("fallback hash = %s, want %s", fallback.Hash(), active.Hash())
	}
	if got := fallback.Custom(promptassets.FileAgent, "Persona"); got != "Keep this known-good guidance." {
		t.Fatalf("fallback persona = %q", got)
	}
}

func TestResolveRuntimePromptSnapshotFallsBackToBuiltInsWithoutActivation(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	cfg := &config.Config{Path: filepath.Join(root, "config.yaml")}
	promptRoot := filepath.Join(root, "prompts")
	if err := os.MkdirAll(promptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptRoot, "agent.md"), []byte("## Unknown\n\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, status := resolveAndActivatePromptSnapshot(cfg, dataDir)
	if status.Source != PromptSourceBuiltIn || status.ActiveErrorKind != "invalid_content" {
		t.Fatalf("status = %+v", status)
	}
	if snapshot.Hash() != promptassets.Empty(promptRoot).Hash() {
		t.Fatalf("snapshot hash = %s, want built-ins", snapshot.Hash())
	}
	if _, err := promptassets.LoadRevision(promptRoot, snapshot.Hash()); err != nil {
		t.Fatalf("built-in fallback revision was not persisted: %v", err)
	}
	if _, err := os.Stat(promptActivationPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("invalid workspace wrote activation record: %v", err)
	}
}

func TestResolveRuntimePromptSnapshotRejectsBroadPermissionsAndUsesLastKnownGood(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	cfg := &config.Config{Path: filepath.Join(root, "config.yaml")}
	promptRoot := filepath.Join(root, "prompts")
	if err := os.MkdirAll(promptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(promptRoot, "agent.md")
	if err := os.WriteFile(agentPath, []byte("## Persona\n\nSafe permissions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownGood, status := resolveAndActivatePromptSnapshot(cfg, dataDir)
	if status.Degraded() {
		t.Fatalf("activation status = %+v", status)
	}
	if err := os.Chmod(agentPath, 0o666); err != nil {
		t.Fatal(err)
	}

	fallback, status := resolveAndActivatePromptSnapshot(cfg, dataDir)
	if status.Source != PromptSourceLastKnownGood || status.ActiveErrorKind != "unsafe_permissions" {
		t.Fatalf("fallback status = %+v", status)
	}
	if fallback.Hash() != knownGood.Hash() {
		t.Fatalf("fallback hash = %s, want %s", fallback.Hash(), knownGood.Hash())
	}
}

func TestResolveRuntimePromptSnapshotDoesNotReuseDifferentRoot(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	firstRoot := filepath.Join(root, "first")
	secondRoot := filepath.Join(root, "second")
	if err := os.MkdirAll(filepath.Join(firstRoot, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstRoot, "prompts", "agent.md"), []byte("## Persona\n\nFirst root.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, status := resolveAndActivatePromptSnapshot(&config.Config{Path: filepath.Join(firstRoot, "config.yaml")}, dataDir); status.Degraded() {
		t.Fatalf("first activation status = %+v", status)
	}
	if err := os.MkdirAll(filepath.Join(secondRoot, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "prompts", "agent.md"), []byte("## Unknown\n\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, status := resolveAndActivatePromptSnapshot(&config.Config{Path: filepath.Join(secondRoot, "config.yaml")}, dataDir)
	if status.Source != PromptSourceBuiltIn || snapshot.Custom(promptassets.FileAgent, "Persona") != "" {
		t.Fatalf("different root reused activation: status=%+v persona=%q", status, snapshot.Custom(promptassets.FileAgent, "Persona"))
	}
}

func TestInspectRuntimePromptSnapshotIsReadOnly(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	cfg := &config.Config{Path: filepath.Join(root, "config.yaml")}

	if _, status := InspectRuntimePromptSnapshot(cfg, dataDir); status.Degraded() {
		t.Fatalf("inspect status = %+v", status)
	}
	if _, err := os.Stat(promptActivationPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("inspection wrote activation record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prompts", ".revisions")); !os.IsNotExist(err) {
		t.Fatalf("inspection wrote revision cache: %v", err)
	}
}
