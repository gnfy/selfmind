package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveScopedPathDefaultsAndBlocksEscape(t *testing.T) {
	root := t.TempDir()
	scope := ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}}

	got, err := resolveScopedPath(scope, "sub/file.txt")
	if err != nil {
		t.Fatalf("resolve relative path: %v", err)
	}
	want := filepath.Join(root, "sub", "file.txt")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if _, err := resolveScopedPath(scope, filepath.Join(root, "..", "outside.txt")); err == nil {
		t.Fatal("expected escape path to fail")
	}
}

func TestWorkspaceScopeMiddlewareMutatesTerminalCWD(t *testing.T) {
	root := t.TempDir()
	tenantID := "person_test"
	cleanup := SetExecutionScope(tenantID, ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}})
	defer cleanup()

	mw := WorkspaceScopeMiddleware()
	var seen string
	exec := mw(func(args map[string]interface{}) (string, error) {
		seen, _ = args["cwd"].(string)
		return "ok", nil
	})

	_, err := exec(map[string]interface{}{
		"_tenant_id": tenantID,
		"_tool_name": "terminal",
		"cwd":        ".",
	})
	if err != nil {
		t.Fatalf("middleware failed: %v", err)
	}
	if seen != root {
		t.Fatalf("cwd = %q, want %q", seen, root)
	}
}

func TestScopePatchContentRewritesPaths(t *testing.T) {
	root := t.TempDir()
	scope := ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: internal/app.go",
		"@@",
		"-old",
		"+new",
		"*** End Patch",
	}, "\n")

	got, err := scopePatchContent(scope, patch)
	if err != nil {
		t.Fatalf("scope patch: %v", err)
	}
	if !strings.Contains(got, filepath.Join(root, "internal", "app.go")) {
		t.Fatalf("patch path was not scoped: %s", got)
	}
}

func TestApprovalProjectRootUsesAllowedSecondaryRoot(t *testing.T) {
	primary := t.TempDir()
	secondary := t.TempDir()
	target := filepath.Join(secondary, "README.md")
	scope := ExecutionScope{WorkspaceRoot: primary, AllowedRoots: []string{primary, secondary}}
	args := map[string]interface{}{"path": target, "_tool_name": "read_file"}

	effective := approvalProjectRoot("/daemon/cwd", scope, args)
	if effective != filepath.Clean(secondary) {
		t.Fatalf("effective root = %q, want %q", effective, filepath.Clean(secondary))
	}
	if dangerous, reason := dangerousToolCall(effective, "read_file", args); dangerous {
		t.Fatalf("allowed secondary-root read classified dangerous: %s", reason)
	}
}
