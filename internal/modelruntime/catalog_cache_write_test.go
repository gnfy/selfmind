package modelruntime

import (
	"os"
	"path/filepath"
	"testing"
)

// The model catalogue cache is not a secret, but what it holds steers which
// models the person is offered. A confined command that could plant a symlink at
// its old temporary name could write that content into any file the daemon's
// user can write, and could replace the cache with its own.
func TestCatalogCacheNeverWritesThroughAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_cache.json")
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(path)
	catalog.writeCache("provider-key", []string{"model-a", "model-b"})
	if got, _ := os.ReadFile(attacker); len(got) != 0 {
		t.Fatalf("the cache was written through a planted symlink: %q", got)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the cache must be a regular file: %v %v", info, err)
	}
	if models, _, _ := catalog.readCacheEntry("provider-key"); len(models) != 2 {
		t.Fatalf("the cache did not round-trip: %v", models)
	}

	// A file the command already holds open at the old name must not become the cache.
	os.Remove(path + ".tmp")
	if err := os.WriteFile(path+".tmp", []byte("held"), 0o666); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path + ".tmp")
	catalog.writeCache("provider-key", []string{"model-c"})
	if after, err := os.Stat(path); err != nil || os.SameFile(before, after) {
		t.Fatalf("the cache became the file the command planted: %v", err)
	}
}
