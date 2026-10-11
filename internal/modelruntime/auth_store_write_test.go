package modelruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OAuth refresh writes a provider's refresh token through writeAuthStore.
// It runs as the daemon, in a directory a confined command with a home-wide
// writable root can also write, so the write must not go through a name the
// command could prepare.

func TestWriteAuthStoreNeverWritesThroughAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := writeAuthStore(path, map[string]interface{}{"providers": map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The names the old write used, and a lock beside it.
	for _, planted := range []string{path + ".tmp", path + ".lock"} {
		if err := os.Symlink(attacker, planted); err != nil {
			t.Fatal(err)
		}
	}
	store := map[string]interface{}{"providers": map[string]interface{}{
		"codex-cli": map[string]interface{}{"refresh_token": "refresh-token-value"},
	}}
	if err := writeAuthStore(path, store); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the refresh token was written through a planted symlink: %q", got)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the store must be a private regular file, not a link to the attacker's: %v %v", info, err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "refresh-token-value") {
		t.Fatalf("store = %s", got)
	}
}

func TestWriteAuthStoreNeverAdoptsAFileHeldAtTheOldTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	held := path + ".tmp"
	if err := os.WriteFile(held, []byte("held"), 0o666); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(held)
	if err := writeAuthStore(path, map[string]interface{}{"providers": map[string]interface{}{"minimax-oauth": map[string]interface{}{"access_token": "t"}}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("the store became the file the command planted, readable through its open descriptor")
	}
}
