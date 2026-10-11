package tools

import (
	"path/filepath"
	"selfmind/internal/tools/sandbox"
	"testing"
)

func TestRelativeRuntimeRootKeepsIsolatedExecutionWorking(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("no sandbox")
	}
	policy := CurrentExecSandboxPolicy()
	SetExecSandbox(true, false, false)
	t.Cleanup(func() { SetExecSandbox(policy.Enabled, policy.Required, policy.AllowNetwork) })
	t.Chdir(t.TempDir())
	tenant, workspace := scratchExecScope(t)
	root, err := filepath.Abs(filepath.Join("state", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	bindTestExecutionRuntime(t, tenant, NewExecutionRuntime(filepath.Join("state", "data"), root, sandbox.PrivateState{}, nil))
	control, err := NewExecuteCommandTool().Execute(map[string]interface{}{"_tenant_id": tenant, "cwd": workspace, "command": "printf OK", "sandbox": "isolated"})
	if err != nil || control != "OK" {
		t.Fatalf("absolute control failed: output=%q err=%v", control, err)
	}
	bindTestExecutionRuntime(t, tenant, NewExecutionRuntime(filepath.Join("state", "data"), filepath.Join("state", "runtime"), sandbox.PrivateState{}, nil))
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{"_tenant_id": tenant, "cwd": workspace, "command": "printf OK", "sandbox": "isolated"})
	if err != nil || out != "OK" {
		t.Fatalf("relative data directory broke isolated command: output=%q error=%v", out, err)
	}
}
