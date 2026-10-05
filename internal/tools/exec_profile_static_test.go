package tools

import (
	"os"
	"path/filepath"
	"selfmind/internal/executionenv"
	"strings"
	"testing"
)

func TestParallelStaticEnvironmentActuallyExecutesInterpreterChecks(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("enforced sandbox unavailable")
	}
	base := fixtureBase(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws/config"), []byte("[default]\nregion=us-east-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
	scope, _, _ := lookupExecutionScope(tenant)
	scope.ParallelWork = true
	scope.SandboxPolicy = &ExecSandboxPolicy{Enabled: true, Required: true}
	cleanup := SetExecutionScope(tenant, scope)
	defer cleanup()
	if err := os.WriteFile(filepath.Join(workspace, "sample.sh"), []byte("echo checked\ncat \"$HOME/.aws/config\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"bash --version", "bash -n sample.sh && echo SYNTAX_OK", "bash sample.sh"} {
		out, err := NewExecuteCommandTool().Execute(map[string]interface{}{"_tenant_id": tenant, "command": command, "cwd": workspace, "sandbox": "isolated"})
		if err != nil {
			t.Fatalf("required isolation failed for %q: %v %s", command, err, out)
		}
		if command == "bash sample.sh" && (!strings.Contains(out, "checked") || !strings.Contains(out, "region=us-east-1")) {
			t.Fatalf("actual script didn't run: %s", out)
		}
	}
}
