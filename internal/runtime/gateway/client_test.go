package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetachedRunArgsDoNotLeakLifecycleFlags(t *testing.T) {
	want := []string{"gateway", "run"}
	if got := detachedRunArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("detachedRunArgs() = %v, want %v", got, want)
	}
}

// TestPickDaemonExecutable pins the daemon-binary resolution order and the
// after-upgrade fallback: a current executable whose path no longer exists
// (npm swaps the package directory out from under a running process) must
// fall back to `selfmind` on PATH instead of fork/exec'ing a deleted path.
func TestPickDaemonExecutable(t *testing.T) {
	// Explicit override always wins, even over an existing current executable.
	if got, err := pickDaemonExecutable("/opt/custom/selfmind", "/whatever", nil); err != nil || got != "/opt/custom/selfmind" {
		t.Fatalf("override: got %q err %v, want the override", got, err)
	}

	// Existing current executable is used as-is.
	current := filepath.Join(t.TempDir(), "selfmind-current")
	if err := os.WriteFile(current, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := pickDaemonExecutable("", current, nil); err != nil || got != current {
		t.Fatalf("existing current: got %q err %v, want %q", got, err, current)
	}

	// Deleted current executable falls back to PATH.
	binDir := t.TempDir()
	fromPath := filepath.Join(binDir, "selfmind")
	if err := os.WriteFile(fromPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	if got, err := pickDaemonExecutable("", filepath.Join(binDir, "gone-after-upgrade"), nil); err != nil || got != fromPath {
		t.Fatalf("deleted current: got %q err %v, want PATH fallback %q", got, err, fromPath)
	}

	// Deleted current + nothing on PATH is an actionable error.
	t.Setenv("PATH", t.TempDir())
	if _, err := pickDaemonExecutable("", "/nonexistent/selfmind", nil); err == nil {
		t.Fatal("deleted current with empty PATH must error")
	}
}
