package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// A separate Git checkout is useful only if the actual process sandbox can
// write its view and cannot write the original checkout through an absolute
// path. This runs the platform backend, not just its policy planner.
func TestManagedGitViewProcessIsConfinedToItsOwnCheckout(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("no isolation backend available on this host")
	}
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "file.txt")
	git("commit", "-qm", "base")
	baseline, err := executionenv.InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := executionenv.EnsureGitView(ctx, filepath.Join(t.TempDir(), "views"), "view_test", baseline)
	if err != nil {
		t.Fatal(err)
	}
	material := execMaterial{WritableRoots: []string{view.Path}, Env: os.Environ()}
	policy := &ExecSandboxPolicy{Enabled: true, Required: true, AllowNetwork: false}
	run := func(args ...string) (string, error) {
		t.Helper()
		cmd, decision, _, err := sandboxedCommandWithMaterialPolicy(ctx, args, material,
			SandboxIsolated, runtime.GOOS, ExecSandboxAvailable(), policy)
		if err != nil {
			return "", err
		}
		if decision.Mode != SandboxIsolated || decision.NetworkShared {
			t.Fatalf("view lost isolation: %+v", decision)
		}
		cmd.Dir = view.Path
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	if output, err := run("/bin/sh", "-c", `printf changed > file.txt`); err != nil {
		t.Fatalf("cannot write inside view: %v: %s", err, output)
	}
	if output, err := run("git", "status", "--porcelain"); err != nil {
		t.Fatalf("Git cannot inspect its managed view: %v: %s", err, output)
	} else if !strings.Contains(output, "file.txt") {
		t.Fatalf("Git did not see the view-local change: %q", output)
	}
	if output, err := run("git", "add", "file.txt"); err != nil {
		t.Fatalf("Git cannot stage a view-local change: %v: %s", err, output)
	}
	if output, err := run("git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "view change"); err != nil {
		t.Fatalf("Git cannot commit a view-local change: %v: %s", err, output)
	}
	escape := filepath.Join(root, "escape.txt")
	if output, err := run("/bin/sh", "-c", `printf escaped > "$1"`, "sh", escape); err == nil {
		t.Fatalf("sandbox wrote into original checkout: %s", output)
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("original checkout was modified: %v", err)
	}

	// Exercise the ordinary terminal execution engine too. Its sandbox policy
	// must come from this Run's scope, including after process material and
	// environment overlays are resolved.
	tenant := "tenant-" + t.Name()
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID: tenant, PersonID: "person", WorkspaceID: "workspace", RunID: "run-view",
		WorkspaceRoot: view.Path, AllowedRoots: []string{view.Path},
		RootBindings: []executionenv.RootBinding{{Path: view.Path, Role: executionenv.RootRolePrimary,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceExecutionView, GitBaseline: &baseline}},
		SandboxPolicy: policy, ApprovalMode: ApprovalFullAuto,
	})
	defer cleanup()
	if output, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant, "command": `printf terminal > terminal.txt && git add terminal.txt`,
		"cwd": view.Path, "timeout": 20,
	}); err != nil {
		t.Fatalf("ordinary terminal cannot stage in isolated view: %v: %s", err, output)
	}
	if output, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant, "command": fmt.Sprintf("printf escaped > %q", escape),
		"cwd": view.Path, "timeout": 20,
	}); err == nil {
		t.Fatalf("ordinary terminal escaped the isolated view: %s", output)
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("ordinary terminal modified original checkout: %v", err)
	}
}
