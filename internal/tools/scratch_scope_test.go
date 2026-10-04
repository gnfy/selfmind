package tools

import (
	"os"
	"path/filepath"
	"selfmind/internal/executionenv"
	"testing"
)

func TestFileToolsShareOnlyExactLeaseScratch(t *testing.T) {
	_, workspace := scratchExecScope(t)
	scratch, err := executionenv.EnsureLeaseScratch("lease-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := executionenv.EnsureLeaseScratch("lease-b")
	if err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{WorkspaceRoot: workspace, LeaseID: "lease-a"}
	if err := os.Symlink(other.TmpDir, filepath.Join(scratch.TmpDir, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path    string
		allowed bool
	}{
		{filepath.Join(scratch.TmpDir, "nested", "proof.json"), true},
		{filepath.Join(other.TmpDir, "proof.json"), false},
		{filepath.Join(scratch.StateDir, "credentials"), false},
		{filepath.Join(scratch.TmpDir, "escape", "proof.json"), false},
	} {
		if got := scopeAllowsPath(scope, tc.path); got != tc.allowed {
			t.Fatalf("path %s allowed=%v want=%v", tc.path, got, tc.allowed)
		}
	}
	if scopeAllowsPath(ExecutionScope{WorkspaceRoot: scope.WorkspaceRoot}, scratch.TmpDir) {
		t.Fatal("scratch granted without a lease")
	}
}
