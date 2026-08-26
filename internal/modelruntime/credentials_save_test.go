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
