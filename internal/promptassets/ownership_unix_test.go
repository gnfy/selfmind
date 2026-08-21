//go:build linux || darwin

package promptassets

import (
	"os"
	"strings"
	"testing"
)

func TestLoadRejectsPromptRootOwnedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing ownership requires root")
	}
	root := t.TempDir()
	if err := os.Chown(root, 65534, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "owned by the current user") {
		t.Fatalf("error = %v", err)
	}
}
