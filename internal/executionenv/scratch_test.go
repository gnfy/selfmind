package executionenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withRuntimeRoot(t *testing.T) string {
	t.Helper()
	// Deliberately NOT t.TempDir(): the runtime root must refuse to live under
	// the system temp directory, and that refusal is itself under test below.
	root := filepath.Join(t.TempDir(), "..", "selfmind-runtime-"+t.Name())
	root = filepath.Clean(root)
	home, err := os.UserHomeDir()
	if err == nil {
		root = filepath.Join(home, ".selfmind-test-runtime", t.Name())
	}
	if err := SetRuntimeRoot(root); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
		runtimeRootMu.Lock()
		runtimeRoot = ""
		runtimeRootMu.Unlock()
	})
	return root
}

// The E4 finding, encoded: isolated execution binds a lease's tmp directory at
// /tmp, and that bind shadows every real path beneath /tmp. A scratch root
// inside /tmp therefore makes $SELFMIND_RUN_TMP point at a path that does not
// exist inside the sandbox.
func TestRuntimeRootRejectsPathsUnderTemp(t *testing.T) {
	if err := SetRuntimeRoot(filepath.Join(os.TempDir(), "selfmind-runtime")); err == nil {
		t.Fatal("a runtime root under the system temp directory must be refused")
	}
	if err := SetRuntimeRoot("/tmp/selfmind-runtime"); err == nil {
		t.Fatal("a runtime root under /tmp must be refused")
	}
	if err := SetRuntimeRoot(""); err == nil {
		t.Fatal("an empty runtime root must be refused")
	}
}

func TestLeaseScratchIsStableAndPrivate(t *testing.T) {
	withRuntimeRoot(t)

	first, err := EnsureLeaseScratch("lease-a")
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent: the same run resolves the same directories on every command.
	second, err := EnsureLeaseScratch("lease-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.TmpDir != second.TmpDir || first.StateDir != second.StateDir {
		t.Fatalf("scratch must be stable across commands: %+v vs %+v", first, second)
	}
	for _, dir := range []string{first.Root, first.TmpDir, first.StateDir} {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s has mode %v, want 0700 (it can hold credential state)", dir, perm)
		}
	}

	// Cross-run isolation: a different lease gets a different tree.
	other, err := EnsureLeaseScratch("lease-b")
	if err != nil {
		t.Fatal(err)
	}
	if other.TmpDir == first.TmpDir {
		t.Fatal("different runs must not share scratch")
	}

	// A file written by one command is visible to the next command of the SAME
	// run — the discontinuity that lost kubeconfigs and password files.
	handoff := filepath.Join(first.TmpDir, "handoff.txt")
	if err := os.WriteFile(handoff, []byte("from command A"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := EnsureLeaseScratch("lease-a")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(again.TmpDir, "handoff.txt"))
	if err != nil || string(data) != "from command A" {
		t.Fatalf("scratch handoff lost: %v %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(other.TmpDir, "handoff.txt")); err == nil {
		t.Fatal("another run must not see this run's scratch file")
	}
}

// $SELFMIND_RUN_TMP is the only cross-mode promise, and TMPDIR follows it so
// tools that honour TMPDIR get run-scoped temp space for free.
func TestScratchEnvOverrides(t *testing.T) {
	withRuntimeRoot(t)
	scratch, err := EnsureLeaseScratch("lease-env")
	if err != nil {
		t.Fatal(err)
	}
	overrides := strings.Join(scratch.EnvOverrides(), " ")
	for _, want := range []string{
		ScratchTmpEnvVar + "=" + scratch.TmpDir,
		ScratchStateEnvVar + "=" + scratch.StateDir,
		"TMPDIR=" + scratch.TmpDir,
	} {
		if !strings.Contains(overrides, want) {
			t.Fatalf("missing override %q in %q", want, overrides)
		}
	}
	if (LeaseScratch{}).EnvOverrides() != nil {
		t.Fatal("an empty scratch must not export overrides")
	}
}

func TestScratchRejectsUnsafeIdentifiers(t *testing.T) {
	withRuntimeRoot(t)
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "lease..1"} {
		if _, err := EnsureLeaseScratch(id); err == nil {
			t.Fatalf("lease id %q must be refused", id)
		}
	}
	if _, err := ToolchainDir("person-1", "../escape"); err == nil {
		t.Fatal("a traversing toolchain key must be refused")
	}
	dir, err := ToolchainDir("person-1", "go-build")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.ToSlash(dir), "toolchain/person-1/go-build") {
		t.Fatalf("unexpected toolchain dir %q", dir)
	}
}

// Toolchain caches are person-level on purpose: a per-run cache would make every
// run a cold build. They must NOT be removed by the run-scratch sweep.
func TestSweepRemovesExpiredScratchOnly(t *testing.T) {
	withRuntimeRoot(t)
	expired, err := EnsureLeaseScratch("lease-old")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := EnsureLeaseScratch("lease-new")
	if err != nil {
		t.Fatal(err)
	}
	active, err := EnsureLeaseScratch("lease-active")
	if err != nil {
		t.Fatal(err)
	}
	toolchain, err := ToolchainDir("person-1", "go-build")
	if err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{expired.Root, active.Root} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := SweepExpiredScratch(24*time.Hour, func(leaseID string) bool {
		return leaseID == "lease-active"
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removal, got %d", removed)
	}
	if _, err := os.Stat(expired.Root); !os.IsNotExist(err) {
		t.Fatal("expired scratch must be removed")
	}
	if _, err := os.Stat(fresh.Root); err != nil {
		t.Fatal("scratch inside the TTL must survive")
	}
	if _, err := os.Stat(active.Root); err != nil {
		t.Fatal("an active run's scratch must never be removed")
	}
	if _, err := os.Stat(toolchain); err != nil {
		t.Fatal("person-level toolchain cache must survive a run-scratch sweep")
	}
}

func TestScratchBytesAndCleanup(t *testing.T) {
	withRuntimeRoot(t)
	scratch, err := EnsureLeaseScratch("lease-size")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch.TmpDir, "blob"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	size, err := ScratchBytes("lease-size")
	if err != nil {
		t.Fatal(err)
	}
	if size < 4096 {
		t.Fatalf("expected at least 4096 bytes, got %d", size)
	}
	if err := CleanupLeaseScratch("lease-size"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch.Root); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the scratch tree")
	}
}
