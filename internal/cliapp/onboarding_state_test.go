package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func TestOnboardingStateCoreReadyTracksBothModelRoutes(t *testing.T) {
	cfg := &config.Config{Models: config.ModelsConfig{
		Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-primary"},
		Auxiliary: config.ModelSelectionConfig{Provider: "openai", Model: "gpt-background"},
	}}
	cfg.Normalize()
	now := time.Now().UTC()
	state := onboardingState{
		Version:        onboardingStateVersion,
		Primary:        onboardingModelState{Provider: "codex-cli", Model: "gpt-primary", VerifiedAt: now},
		Auxiliary:      onboardingModelState{Provider: "openai", Model: "gpt-background", VerifiedAt: now},
		BackgroundMode: "on-demand", GatewayVerifiedAt: now,
		WorkspaceID: "ws-1", WorkspacePath: "/work/project", WorkspaceTrusted: true,
		ApprovalMode: "smart",
	}
	if !state.coreReady(cfg) {
		t.Fatal("complete receipt should be ready")
	}
	cfg.Models.Auxiliary.Model = "new-background"
	if state.coreReady(cfg) {
		t.Fatal("changed background model must invalidate the setup receipt")
	}
}

func TestOnboardingStatePersistsFirstSuccessfulTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".selfmind", "onboarding.json")
	state := onboardingState{BackgroundMode: "on-demand"}
	state.recordFirstTask()
	if err := saveOnboardingState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOnboardingState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.FirstTaskCompleted || loaded.FirstTaskCompletedAt.IsZero() || loaded.Version != onboardingStateVersion {
		t.Fatalf("loaded receipt = %+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("receipt mode = %o, want 600", got)
	}
}

func TestOnboardingWorkspaceRequiresProjectDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if !onboardingWorkspaceNeedsExplicitChoice(home) || !onboardingWorkspaceNeedsExplicitChoice(string(filepath.Separator)) {
		t.Fatal("home and filesystem root must never be accepted as implicit workspaces")
	}
	if onboardingWorkspaceNeedsExplicitChoice(project) {
		t.Fatal("project directory should be accepted")
	}
}

func TestOnboardingModelFastPathConfirmsAndProbesBothRoutes(t *testing.T) {
	cfg := &config.Config{
		Path: filepath.Join(t.TempDir(), "config.yaml"),
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "openai", Model: "gpt-primary"},
			Auxiliary: config.ModelSelectionConfig{Provider: "openai", Model: "gpt-background"},
		},
		Providers: config.ProvidersConfig{OpenAI: config.ProviderEndpoint{APIKey: "test-key"}},
	}
	cfg.Normalize()
	var stdout, stderr bytes.Buffer
	roles := []string{}
	app := &App{
		ctx: context.Background(), stdin: strings.NewReader("y\n"), stdout: &stdout, stderr: &stderr,
		interactive: true,
		onboardingProbe: func(_ *config.Config, role string) error {
			roles = append(roles, role)
			return nil
		},
	}
	state := onboardingState{}
	got, code := app.runOnboardingModelStep(cfg, &state, onboardingOptions{})
	if code != 0 || got == nil || stderr.Len() != 0 {
		t.Fatalf("model step cfg=%v code=%d stderr=%q", got != nil, code, stderr.String())
	}
	if strings.Join(roles, ",") != "primary,auxiliary" || !state.matchesModels(cfg) {
		t.Fatalf("roles=%v state=%+v", roles, state)
	}
	for _, expected := range []string{"Primary:", "gpt-primary", "Background:", "gpt-background", "Continue with these models?"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("model screen missing %q: %q", expected, stdout.String())
		}
	}
}

func TestOnboardingCopiesAcceptedEnvironmentCredentialForDaemon(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	t.Setenv("OPENAI_API_KEY", "environment-secret")
	cfg := &config.Config{
		Path:   filepath.Join(dir, "config.yaml"),
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{Provider: "openai", Model: "gpt-test"}},
		Auth:   config.AuthConfig{CredentialsFile: authPath},
	}
	cfg.Normalize()
	app := &App{
		ctx: context.Background(), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		onboardingProbe: func(*config.Config, string) error { return nil },
	}
	state := onboardingState{}
	if _, code := app.runOnboardingModelStep(cfg, &state, onboardingOptions{NonInteractive: true}); code != 0 {
		t.Fatalf("model step code = %d", code)
	}
	credential := modelruntime.NewCredentialStore(authPath).Resolve("openai")
	if credential.Token != "environment-secret" {
		t.Fatalf("stored credential = %q", credential.Token)
	}
}

func TestRecommendedModelMovesConfiguredSecretOutOfConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, key := range []string{
		"CODEX_ACCESS_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN",
		"GEMINI_OAUTH_ACCESS_TOKEN", "GOOGLE_OAUTH_ACCESS_TOKEN", "QWEN_ACCESS_TOKEN", "QWEN_API_KEY",
	} {
		t.Setenv(key, "")
	}
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	authPath := filepath.Join(home, ".selfmind", "auth.json")
	cfg := &config.Config{
		Path: configPath, Auth: config.AuthConfig{CredentialsFile: authPath},
		Providers: config.ProvidersConfig{OpenAI: config.ProviderEndpoint{APIKey: "configured-secret"}},
	}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	app := &App{ctx: context.Background(), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	applied, err := app.applyRecommendedReadyPrimary(cfg)
	if err != nil || !applied {
		t.Fatalf("apply recommended = %v, %v", applied, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("configured-secret")) {
		t.Fatalf("config still contains the provider secret:\n%s", data)
	}
	if got := modelruntime.NewCredentialStore(authPath).Resolve("openai").Token; got != "configured-secret" {
		t.Fatalf("auth-store token = %q", got)
	}
}
