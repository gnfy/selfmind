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
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustTrusted, Grants: grants,
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
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustUntrusted, Grants: grants,
	})
	if observationOnlyExec("terminal", args) {
		t.Fatal("untrusted workspace must not redeem an observation profile")
	}
	cleanup()
	cleanup = SetExecutionScope("person-1", ExecutionScope{
		TenantID: "tenant-1", PersonID: "person-1", WorkspaceID: "ws-1", WorkspaceRoot: root,
		AllowedRoots: []string{root}, TrustLevel: executionenv.TrustTrusted, Grants: grants,
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
