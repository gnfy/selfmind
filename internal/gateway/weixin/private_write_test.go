package weixin

import (
	"os"
	"path/filepath"
	"testing"
)

// A chat account's bot token and its context tokens are secrets written beside
// each other in a directory a confined command may be able to write. Neither
// write may go through a name the command could prepare.

func TestAccountSecretsNeverWriteThroughAPlantedTempName(t *testing.T) {
	home := t.TempDir()
	dir := accountDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	creds := accountFile(home, "bot")
	if err := os.Symlink(attacker, creds+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredentials(home, &Credentials{AccountID: "bot", Token: "bot-secret-token"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the bot token was written through a planted symlink: %q", got)
	}
	if info, err := os.Lstat(creds); err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the credentials must be a private regular file: %v %v", info, err)
	}
	if loaded, err := LoadCredentials(home, "bot"); err != nil || loaded.Token != "bot-secret-token" {
		t.Fatalf("credentials did not round-trip: %+v %v", loaded, err)
	}

	tokens := filepath.Join(dir, "bot.context-tokens.json")
	if err := os.Symlink(attacker, tokens+".tmp"); err != nil {
		t.Fatal(err)
	}
	NewContextTokenStore(home).Set("bot", "peer", "context-secret-token")
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("a context token was written through a planted symlink: %q", got)
	}
	restored := NewContextTokenStore(home)
	restored.Restore("bot")
	if restored.Get("bot", "peer") != "context-secret-token" {
		t.Fatal("the context token did not persist")
	}
}
