package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Item 8: a background start is host execution by contract, so it follows the
// same boundary rule as a foreground call. exec_sandbox.required forbids the
// host; the foreground call was refused while the background one ran there.
func TestBackgroundHostStartHonoursTheRequiredPolicy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		processPolicy [3]bool
		scopePolicy   *ExecSandboxPolicy
	}{
		{name: "daemon-policy", processPolicy: [3]bool{true, true, false}},
		// The same invariant from the Run's frozen policy.
		{name: "run-policy", processPolicy: [3]bool{true, false, false}, scopePolicy: &ExecSandboxPolicy{Enabled: true, Required: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExecSandboxPolicy(t, tc.processPolicy[0], tc.processPolicy[1], tc.processPolicy[2])
			dir := canonicalPath(t, t.TempDir())
			marker := filepath.Join(dir, "background-ran")
			tenant := "tenant-background-" + tc.name
			t.Cleanup(SetExecutionScope(tenant, ExecutionScope{
				TenantID: tenant, PersonID: "person-background", WorkspaceRoot: dir, AllowedRoots: []string{dir},
				SandboxPolicy: tc.scopePolicy,
			}))
			out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
				"_tenant_id": tenant, "_tool_name": "terminal", "command": "touch " + marker, "cwd": dir,
				"background": true, "sandbox": string(SandboxHost),
			})
			requireNotDispatched(t, err)
			if strings.Contains(out, "Started background process") {
				t.Fatalf("a host start was launched under the required policy: %s", out)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatal("the background command ran on the host")
			}
		})
	}

	// The constraint that must change the result: isolation is not required.
	withExecSandboxPolicy(t, true, false, false)
	dir := canonicalPath(t, t.TempDir())
	tenant := "tenant-background-allowed"
	t.Cleanup(SetExecutionScope(tenant, ExecutionScope{
		TenantID: tenant, PersonID: "person-background", WorkspaceRoot: dir, AllowedRoots: []string{dir},
	}))
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant, "_tool_name": "terminal", "command": "true", "cwd": dir,
		"background": true, "sandbox": string(SandboxHost),
	})
	if err != nil || !strings.Contains(out, "Started background process") {
		t.Fatalf("an approved host start must launch when isolation is not required: %v (%s)", err, out)
	}
}
