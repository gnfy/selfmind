package gateway

import (
	"os"
	"testing"
)

func TestEnsureLocalControlTokenIsStableAndPrivate(t *testing.T) {
	dataDir := t.TempDir()
	first, err := EnsureLocalControlToken(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureLocalControlToken(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("tokens are not stable: %q %q", first, second)
	}
	info, err := os.Stat(ResolvePaths(dataDir).LocalControlTokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("local control token mode = %o", info.Mode().Perm())
	}
}
