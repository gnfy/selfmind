package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
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

func TestResolveScopedPathBlocksSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	scope := ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}}
	if _, err := resolveScopedPath(scope, filepath.Join("outside", "new.txt")); err == nil {
		t.Fatal("path through an out-of-scope symlink must be rejected")
	}
}

func TestResolveScopedPathAllowsSymlinkedWorkspaceRoot(t *testing.T) {
	physical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	scope := ExecutionScope{WorkspaceRoot: alias, AllowedRoots: []string{alias}}
	got, err := resolveScopedPath(scope, "new.txt")
	if err != nil {
		t.Fatalf("symlinked workspace root rejected: %v", err)
	}
	if got != filepath.Join(alias, "new.txt") {
		t.Fatalf("path = %q", got)
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

func TestIsolatedViewRejectsUnclaimedExternalTools(t *testing.T) {
	root := t.TempDir()
	person := "isolated-external"
	cleanup := SetExecutionScope(person, ExecutionScope{PersonID: person, RunID: "isolated-run", WorkspaceRoot: root,
		AllowedRoots: []string{root}, RootBindings: []executionenv.RootBinding{{Path: root, Source: executionenv.RootSourceExecutionView}}})
	defer cleanup()
	calls := 0
	execute := WorkspaceScopeMiddleware()(func(map[string]interface{}) (string, error) {
		calls++
		return "ok", nil
	})
	for _, policy := range []toolExecutionPolicy{
		{Origin: ToolSchemaOriginExternal, ReadOnly: false},
		{Origin: ToolSchemaOriginExternal, ReadOnly: true},
		{Origin: ToolSchemaOriginBuiltin, OperationClasses: []OperationClass{OpClassNetwork}},
	} {
		if _, err := execute(map[string]interface{}{"_tenant_id": person, "_tool_name": "remote_action", toolExecutionPolicyArg: policy}); err == nil {
			t.Fatalf("unclaimed external effect ran under policy %+v", policy)
		}
	}
	if calls != 0 {
		t.Fatalf("unclaimed tool executed %d times", calls)
	}
	if _, err := execute(map[string]interface{}{"_tenant_id": person, "_tool_name": "terminal", "sandbox": "host",
		toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin}}); err == nil || calls != 0 {
		t.Fatalf("host escape reached executor: calls=%d err=%v", calls, err)
	}
	if _, err := execute(map[string]interface{}{"_tenant_id": person, "_tool_name": "read_file", "path": "file.txt",
		toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin, ReadOnly: true}}); err != nil || calls != 1 {
		t.Fatalf("local read blocked: calls=%d err=%v", calls, err)
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

// TestWorkspaceScopeMiddlewareScopesVisionAnalyze pins the vision_analyze
// scope fix: the tool's local-path branch used to os.ReadFile ANY path,
// bypassing AllowedRoots entirely. Local refs (bare paths, file://) must now
// resolve inside the scope like read_file; remote http(s) URLs pass through
// untouched to the tool's own SSRF/egress handling.
func TestWorkspaceScopeMiddlewareScopesVisionAnalyze(t *testing.T) {
	root := t.TempDir()
	tenantID := "person_vision"
	cleanup := SetExecutionScope(tenantID, ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}})
	defer cleanup()

	mw := WorkspaceScopeMiddleware()
	var seen string
	exec := mw(func(args map[string]interface{}) (string, error) {
		seen, _ = args["image_url"].(string)
		return "ok", nil
	})

	// In-scope relative path resolves against the workspace root.
	if _, err := exec(map[string]interface{}{
		"_tenant_id": tenantID, "_tool_name": "vision_analyze",
		"image_url": "shots/a.png", "question": "q",
	}); err != nil {
		t.Fatalf("in-scope path failed: %v", err)
	}
	if want := filepath.Join(root, "shots", "a.png"); seen != want {
		t.Fatalf("image_url = %q, want %q", seen, want)
	}

	// Out-of-scope absolute path is rejected before the tool runs.
	if _, err := exec(map[string]interface{}{
		"_tenant_id": tenantID, "_tool_name": "vision_analyze",
		"image_url": "/etc/passwd.png", "question": "q",
	}); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("out-of-scope path must fail with escape error, got %v", err)
	}

	// Remote URL passes through untouched.
	seen = ""
	if _, err := exec(map[string]interface{}{
		"_tenant_id": tenantID, "_tool_name": "vision_analyze",
		"image_url": "https://example.com/a.png", "question": "q",
	}); err != nil {
		t.Fatalf("remote URL must not be scoped: %v", err)
	}
	if seen != "https://example.com/a.png" {
		t.Fatalf("remote URL mutated to %q", seen)
	}
}

// A delegated sub-agent's context drops the parent's scope key, but its trusted
// invocation scope still names the parent's run, so its calls resolve that
// run's workspace. A call that names a run never borrows the person-level
// scope, which may belong to another execution; a filesystem call whose run
// scope is gone is refused rather than run against the daemon's directory.
func TestDelegatedCallsResolveTheirRunScopeOrNone(t *testing.T) {
	workspace := t.TempDir()
	defer SetExecutionScope("person_scope", ExecutionScope{
		PersonID: "person_scope", RunID: "run_live", WorkspaceRoot: workspace, AllowedRoots: []string{workspace},
	})()
	parentCtx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run_live"))
	delegated := func(tenant, runID, tool string) map[string]interface{} {
		return map[string]interface{}{
			"_context":   kernel.ForkDelegationContext(parentCtx),
			"_tenant_id": tenant,
			"_tool_name": tool,
			"_invocation_scope": kernel.ToolInvocationScope{
				PersonID: "person_scope", RunID: runID, ExecutionScopeKey: ExecutionScopeKeyForRun(runID),
			},
		}
	}
	var seen string
	exec := WorkspaceScopeMiddleware()(func(args map[string]interface{}) (string, error) {
		seen, _ = args["path"].(string)
		return "ran", nil
	})
	call := func(args map[string]interface{}, path string) error {
		seen = ""
		args["path"] = path
		_, err := exec(args)
		return err
	}

	if err := call(delegated("system", "run_live", "read_file"), "notes.txt"); err != nil || seen != filepath.Join(workspace, "notes.txt") {
		t.Fatalf("delegated relative path: seen=%q err=%v", seen, err)
	}
	if err := call(delegated("system", "run_live", "read_file"), "/etc/hosts"); err == nil || seen != "" {
		t.Fatalf("a delegated escape was not refused: seen=%q err=%v", seen, err)
	}
	if err := call(delegated("person_scope", "run_gone", "read_file"), "notes.txt"); err == nil || seen != "" {
		t.Fatalf("a call for a run without a scope ran under another scope: seen=%q err=%v", seen, err)
	}
	if err := call(delegated("person_scope", "run_gone", "update_plan"), "notes.txt"); err != nil || seen != "notes.txt" {
		t.Fatalf("a tool the middleware does not confine was refused: seen=%q err=%v", seen, err)
	}
	if err := call(map[string]interface{}{"_tenant_id": "person_scope", "_tool_name": "read_file"}, "notes.txt"); err != nil || seen != filepath.Join(workspace, "notes.txt") {
		t.Fatalf("a call without a run no longer resolves by person: seen=%q err=%v", seen, err)
	}
}
