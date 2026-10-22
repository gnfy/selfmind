package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The saved configuration holds every provider key and chat token, so it is
// written as the other secret stores are: through a fresh exclusive temporary
// file, never through a name a confined command could have planted.
func TestSaveConfigNeverWritesThroughAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Path:      path,
		Models:    ModelsConfig{Primary: ModelSelectionConfig{Provider: "openai", Model: "gpt-test"}},
		Providers: ProvidersConfig{OpenAI: ProviderEndpoint{APIKey: "sk-secret-provider-key"}},
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the configuration was written through a planted symlink: %q", got)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the saved configuration must be a private regular file: %v %v", info, err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "gpt-test") {
		t.Fatalf("saved config = %s", got)
	}

	// A file the command already holds open at the old name must not become the
	// configuration.
	held := path + ".tmp"
	os.Remove(held)
	if err := os.WriteFile(held, []byte("held"), 0o666); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(held)
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if os.SameFile(before, after) {
		t.Fatal("the configuration became the file the command planted")
	}
}
