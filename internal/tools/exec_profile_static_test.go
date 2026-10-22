package tools

import (
	"os"
	"path/filepath"
	"reflect"
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

// The host's authentication needs writable state that a mountless backend
// refuses, so pulling in the host inventory would block the command outright.
// A leading assignment must prepare and run exactly like the bare command.
func TestParallelIsolatedLeadingAssignmentPreparesLikeTheBareCommand(t *testing.T) {
	base := fixtureBase(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws/config"), []byte("[default]\nsso_session=work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
	scope, _, _ := lookupExecutionScope(tenant)
	scope.ParallelWork = true
	scope.SandboxPolicy = &ExecSandboxPolicy{Enabled: true, Required: true}
	cleanup := SetExecutionScope(tenant, scope)
	defer cleanup()
	for _, pair := range [][2]string{
		{"echo hi", "FOO=1 echo hi"},
		{"printf hi", "LC_ALL=C TZ=UTC printf hi"},
	} {
		bare := execMaterialForArgs(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": pair[0]}, workspace)
		assigned := execMaterialForArgs(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": pair[1]}, workspace)
		if !reflect.DeepEqual(preparationView(assigned), preparationView(bare)) {
			t.Fatalf("%q prepared differently from %q:\n%+v\n%+v", pair[1], pair[0], preparationView(assigned), preparationView(bare))
		}
		if !ExecSandboxAvailable() {
			continue
		}
		for _, command := range pair {
			out, err := NewExecuteCommandTool().Execute(map[string]interface{}{"_tenant_id": tenant, "command": command, "cwd": workspace, "sandbox": "isolated"})
			if err != nil || !strings.Contains(out, "hi") {
				t.Fatalf("required isolation refused %q: %v %s", command, err, out)
			}
		}
	}
	// Executing an unknown script still needs the host's state.
	opaque := execMaterialForArgs(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": "FOO=1 ./deploy.sh"}, workspace)
	if len(opaque.ProfilesFromInventory) == 0 {
		t.Fatal("an executed script lost its host inventory")
	}
}
