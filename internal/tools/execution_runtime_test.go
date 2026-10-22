//go:build !windows

package tools

import (
	"context"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
	"testing"
	"time"
)

func TestExecutionRuntimeKeepsItsOwnProcessReceipts(t *testing.T) {
	first, second := &recordedGroups{}, &recordedGroups{}
	tenant, workspace := scratchExecScope(t)
	firstRuntime := NewExecutionRuntime("one", executionenv.RuntimeRoot(), sandbox.PrivateState{}, first)
	bindTestExecutionRuntime(t, tenant, firstRuntime)
	secondRuntime := NewExecutionRuntime("two", executionenv.RuntimeRoot(), sandbox.PrivateState{}, second)
	secondTenant, secondWorkspace := tenant+"-second", t.TempDir()
	secondScope, ok, _ := lookupExecutionScope(tenant)
	if !ok {
		t.Fatal("first execution scope missing")
	}
	secondScope.TenantID = secondTenant
	secondScope.Runtime = secondRuntime
	secondScope.WorkspaceRoot = secondWorkspace
	secondScope.AllowedRoots = []string{secondWorkspace}
	secondScope.LeaseID += "-second"
	t.Cleanup(SetExecutionScope(secondTenant, secondScope))
	if output, err := NewExecuteCommandTool().Execute(map[string]interface{}{"_tenant_id": secondTenant, "command": "echo $$", "cwd": secondWorkspace, "sandbox": "host"}); err != nil {
		t.Fatalf("second command: %s %v", output, err)
	}
	// The first invocation's exact scope wins over an inherited runtime context.
	if output, err := NewExecuteCommandTool().Execute(map[string]interface{}{"_tenant_id": tenant, "_context": WithExecutionRuntime(context.Background(), secondRuntime), "command": "echo $$", "cwd": workspace, "sandbox": "host"}); err != nil {
		t.Fatalf("command: %s %v", output, err)
	}
	if first.count() != 1 || second.count() != 1 {
		t.Fatalf("one runtime stole another's process receipt: first=%d second=%d", first.count(), second.count())
	}
}

func TestExecutionRuntimeWaitsOnlyForItsOwnRepairs(t *testing.T) {
	first := NewExecutionRuntime("one", t.TempDir(), sandbox.PrivateState{}, nil)
	second := NewExecutionRuntime("two", t.TempDir(), sandbox.PrivateState{}, nil)
	first.trackHostStateCommand(1)
	defer first.trackHostStateCommand(-1)
	if first.WaitForHostStateRepairs(time.Millisecond) {
		t.Fatal("the active repair was not retained")
	}
	if !second.WaitForHostStateRepairs(time.Millisecond) {
		t.Fatal("an unrelated runtime was held by another runtime's repair")
	}
}

func bindTestExecutionRuntime(t *testing.T, tenant string, runtime *ExecutionRuntime) {
	t.Helper()
	scope, ok, _ := lookupExecutionScope(tenant)
	if !ok {
		t.Fatal("fixture execution scope missing")
	}
	scope.Runtime = runtime
	t.Cleanup(SetExecutionScope(tenant, scope))
}
