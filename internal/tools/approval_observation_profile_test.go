package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

func TestHashBoundObservationScriptProfile(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "scripts", "inspect.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("print('read only')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rule, err := BuildObservationScriptRule(ObservationScriptProfile{
		WorkspaceID: "ws-1", ScriptPath: script, ArgvPrefix: []string{"--mode", "inspect"}, AllowTrailing: true,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	grants := newFakeGrantStore()
	if err := grants.GrantApproval(context.Background(), "person", "tenant-1", "person-1", "person-1", rule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	cleanup := SetExecutionScope("person-1", ExecutionScope{
		TenantID: "tenant-1", PersonID: "person-1", WorkspaceID: "ws-1", WorkspaceRoot: root,
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustTrusted, Grants: grants, StandingGrants: InteractiveStandingGrants(),
	})
	defer cleanup()
	args := map[string]interface{}{
		"_tenant_id": "person-1", "_tool_name": "terminal", "cwd": root,
		"command": "python3 scripts/inspect.py --mode inspect RUQX-511",
	}
	if !observationOnlyExec("terminal", args) {
		t.Fatal("registered unchanged script should be observation-only")
	}
	if err := os.WriteFile(script, []byte("print('changed')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if observationOnlyExec("terminal", args) {
		t.Fatal("content change must invalidate the observation profile")
	}
}

func TestAWSCodeBuildProjectLookupIsCredentialSafeObservation(t *testing.T) {
	args := map[string]interface{}{
		"_tool_name":         "terminal",
		"command":            "aws --region us-east-1 codebuild batch-get-projects --names api www",
		credentialReadArgKey: true,
	}
	if !observationOnlyExec("terminal", args) {
		t.Fatal("aws codebuild batch-get-projects should be a proven read-only observation")
	}
}

func TestObservationScriptProfileRequiresTrustedWorkspaceAndDeclaredEnvironment(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "inspect.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rule, err := BuildObservationScriptRule(ObservationScriptProfile{
		WorkspaceID: "ws-1", ScriptPath: script, AllowTrailing: true,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	grants := newFakeGrantStore()
	if err := grants.GrantApproval(context.Background(), "person", "tenant-1", "person-1", "person-1", rule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{
		"_tenant_id": "person-1", "_tool_name": "terminal", "cwd": root,
		"command": "bash inspect.sh RUQX-511", "_network_shared": true,
	}
	cleanup := SetExecutionScope("person-1", ExecutionScope{
		TenantID: "tenant-1", PersonID: "person-1", WorkspaceID: "ws-1", WorkspaceRoot: root,
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustUntrusted, Grants: grants, StandingGrants: InteractiveStandingGrants(),
	})
	if observationOnlyExec("terminal", args) {
		t.Fatal("untrusted workspace must not redeem an observation profile")
	}
	cleanup()
	cleanup = SetExecutionScope("person-1", ExecutionScope{
		TenantID: "tenant-1", PersonID: "person-1", WorkspaceID: "ws-1", WorkspaceRoot: root,
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustTrusted, Grants: grants, StandingGrants: InteractiveStandingGrants(),
	})
	defer cleanup()
	if observationOnlyExec("terminal", args) {
		t.Fatal("network-bearing execution must not redeem a no-network profile")
	}
}

func TestObservationScriptProfileRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "inspect.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "inspect.sh")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := BuildObservationScriptRule(ObservationScriptProfile{
		WorkspaceID: "ws", ScriptPath: "inspect.sh",
	}, workspace); err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("symlink escape accepted: %v", err)
	}
}

// The shell follows a symlink before it applies a later "..": from the root,
// link/../scripts/inspect.sh runs the inspect.sh beside the link's target,
// which the owner never reviewed, though the path cleans as text to the
// approved script. Only a path that reaches the approved file redeems the
// profile, whether the script runs directly or through its interpreter, and
// a profile cannot be registered through such a path.
func TestObservationScriptProfileReadsDotDotAsTheShellDoes(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"scripts", "config", "other/deep", "other/scripts"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		"scripts/inspect.sh":       "#!/bin/sh\necho reviewed\n",
		"other/scripts/inspect.sh": "#!/bin/sh\necho unreviewed\n",
		"scripts/inspect.py":       "print('reviewed')\n",
		"other/scripts/inspect.py": "print('unreviewed')\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "other", "deep"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	grants := newFakeGrantStore()
	for _, script := range []string{"scripts/inspect.sh", "scripts/inspect.py"} {
		rule, err := BuildObservationScriptRule(ObservationScriptProfile{
			WorkspaceID: "ws-1", ScriptPath: script, AllowTrailing: true,
		}, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := grants.GrantApproval(context.Background(), "person", "tenant-1", "person-1", "person-1", rule.Key, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := SetExecutionScope("person-1", ExecutionScope{
		TenantID: "tenant-1", PersonID: "person-1", WorkspaceID: "ws-1", WorkspaceRoot: root,
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustTrusted, Grants: grants, StandingGrants: InteractiveStandingGrants(),
	})
	defer cleanup()
	for _, call := range []struct {
		cwd, command string
		want         bool
	}{
		{root, "./scripts/inspect.sh RUQX-511", true},
		{root, "config/../scripts/inspect.sh RUQX-511", true},
		{root, "python3 scripts/inspect.py RUQX-511", true},
		{root, "link/../scripts/inspect.sh RUQX-511", false},
		{root, "python3 link/../scripts/inspect.py RUQX-511", false},
		{root, root + "/link/../scripts/inspect.sh RUQX-511", false},
		// From the linked directory, ".." is other/, not the workspace root.
		{filepath.Join(root, "link"), "../scripts/inspect.sh RUQX-511", false},
	} {
		args := map[string]interface{}{"_tenant_id": "person-1", "_tool_name": "terminal", "cwd": call.cwd, "command": call.command}
		if got := observationOnlyExec("terminal", args); got != call.want {
			t.Errorf("%q in %s observation-only = %v, want %v", call.command, call.cwd, got, call.want)
		}
	}
	if _, err := BuildObservationScriptRule(ObservationScriptProfile{
		WorkspaceID: "ws-1", ScriptPath: "link/../scripts/inspect.sh", AllowTrailing: true,
	}, root); err == nil {
		t.Fatal("a profile reached through a symlink and \"..\" was registered")
	}
}
