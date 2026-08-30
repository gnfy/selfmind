package modelruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/platform/config"
)

func TestCredentialStoreSaveAPIKeyMergesAtomicallyAndRestrictsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "auth.json")
	store := NewCredentialStore(path)
	if err := store.SaveAPIKey("OpenAI", "first-secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAPIKey("custom:Lab", "second-secret"); err != nil {
		t.Fatal(err)
	}
	if got := store.Resolve("openai").Token; got != "first-secret" {
		t.Fatalf("openai token = %q", got)
	}
	if got := store.Resolve("custom:lab").Token; got != "second-secret" {
		t.Fatalf("custom token = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credential mode = %o, want 600", got)
	}
}

func TestCredentialStoreStagesCommitsAndRollsBackAPIKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := NewCredentialStore(path)
	if err := store.SaveAPIKey("openai", "old-secret"); err != nil {
		t.Fatal(err)
	}
	stage, err := store.StageAPIKeys("", map[string]string{
		"openai":   "new-secret",
		"deepseek": "deep-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stage == "" {
		t.Fatal("expected opaque credential stage id")
	}
	if got := store.Resolve("openai").Token; got != "old-secret" {
		t.Fatalf("staging changed active credential: %q", got)
	}
	if got := store.ResolveStaged(stage, "openai").Token; got != "new-secret" {
		t.Fatalf("staged credential = %q", got)
	}
	if err := store.CommitStage(stage); err != nil {
		t.Fatal(err)
	}
	if got := store.Resolve("openai").Token; got != "new-secret" {
		t.Fatalf("committed credential = %q", got)
	}
	if got := store.Resolve("deepseek").Token; got != "deep-secret" {
		t.Fatalf("committed new provider credential = %q", got)
	}
	if err := store.RollbackStage(stage); err != nil {
		t.Fatal(err)
	}
	if got := store.Resolve("openai").Token; got != "old-secret" {
		t.Fatalf("rolled-back credential = %q", got)
	}
	if got := store.Resolve("deepseek").Token; got != "" {
		t.Fatalf("rolled-back new provider credential remained: %q", got)
	}
}

func TestCredentialStoreMergesAnExistingUncommittedStage(t *testing.T) {
	store := NewCredentialStore(filepath.Join(t.TempDir(), "auth.json"))
	stage, err := store.StageAPIKeys("", map[string]string{"openai": "one"})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := store.StageAPIKeys(stage, map[string]string{"anthropic": "two"})
	if err != nil {
		t.Fatal(err)
	}
	if merged != stage {
		t.Fatalf("merged stage id = %q, want %q", merged, stage)
	}
	if got := store.ResolveStaged(stage, "openai").Token; got != "one" {
		t.Fatalf("openai staged credential = %q", got)
	}
	if got := store.ResolveStaged(stage, "anthropic").Token; got != "two" {
		t.Fatalf("anthropic staged credential = %q", got)
	}
	if err := store.DiscardStage(stage); err != nil {
		t.Fatal(err)
	}
	if got := store.ResolveStaged(stage, "openai").Token; got != "" {
		t.Fatalf("discarded credential remained: %q", got)
	}
}

func TestResolverUsesStoredCustomProviderCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := NewCredentialStore(path).SaveAPIKey("custom:Lab", "stored-secret"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{Provider: "custom:Lab", Model: "lab-model"}},
		Auth:   config.AuthConfig{CredentialsFile: path},
		Providers: config.ProvidersConfig{Custom: []config.CustomProvider{{
			Name: "Lab", BaseURL: "https://lab.example/v1", Protocol: ProtocolOpenAICompatible, Model: "lab-model",
		}}},
	}
	cfg.Normalize()
	runtime, err := NewResolver(cfg).Resolve(context.Background(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.APIKey != "stored-secret" || runtime.CredentialSource != "selfmind-auth:"+path {
		t.Fatalf("resolved credential = token:%q source:%q", runtime.APIKey, runtime.CredentialSource)
	}
}
