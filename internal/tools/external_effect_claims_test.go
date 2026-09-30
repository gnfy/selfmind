package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

type claimTestTool struct {
	BaseTool
	target           string
	completeOnReturn bool
	calls            int
}

type externalClaimTestTool struct{ *claimTestTool }

func (*externalClaimTestTool) SchemaOrigin() ToolSchemaOrigin { return ToolSchemaOriginExternal }

func newClaimTestTool(name, target string, complete bool) *claimTestTool {
	tool := &claimTestTool{target: target, completeOnReturn: complete}
	tool.BaseTool = BaseTool{name: name, description: "claim test", schema: ToolSchema{Type: "object",
		AdditionalProperties: rejectAdditionalProperties()}, metadata: ToolMetadata{
		Category: "network", OperationClasses: []OperationClass{OpClassNetwork},
	}}
	return tool
}

func (t *claimTestTool) Execute(map[string]interface{}) (string, error) {
	t.calls++
	return "remote operation accepted", nil
}

func (t *claimTestTool) ExternalEffectTargets(map[string]interface{}) ([]string, bool, error) {
	return []string{t.target}, t.completeOnReturn, nil
}

func TestExternalEffectMiddlewareUsesTrustedTargetAndRetainsAsyncClaim(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	var runs []*control.Run
	for _, title := range []string{"A", "B", "C"} {
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: title, Channel: title})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRunWithOptions(ctx, task, title, title, control.StartRunOptions{MaxActiveRuns: 3})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	registry := NewRegistry()
	registry.UseResultMiddleware(ExternalEffectClaimMiddleware(store))
	prod := newClaimTestTool("deploy_production", "deploy:production", false)
	stage := newClaimTestTool("deploy_staging", "deploy:staging", true)
	registry.Register(prod)
	registry.Register(stage)
	for _, run := range runs {
		key := ExecutionScopeKeyForRun(run.ID)
		cleanup := SetExecutionScope(key, ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID,
			RunID: run.ID, WorkspaceID: "workspace", WorkspaceRoot: t.TempDir(), ParallelWork: true})
		t.Cleanup(cleanup)
	}
	dispatch := func(run *control.Run, tool, call string) (string, error) {
		return registry.Dispatch(tool, map[string]interface{}{
			"_tool_call_id":     call,
			"_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(run.ID)},
		})
	}
	if _, err := dispatch(runs[0], prod.Name(), "call-a"); err != nil || prod.calls != 1 {
		t.Fatalf("first known target did not execute once: calls=%d err=%v", prod.calls, err)
	}
	if _, err := dispatch(runs[1], prod.Name(), "call-b"); err == nil || !strings.Contains(err.Error(), "occupied") || prod.calls != 1 {
		t.Fatalf("conflicting target executed or lacked explanation: calls=%d err=%v", prod.calls, err)
	} else {
		var boundary interface{ ToolRunPauseStatus() string }
		if !errors.As(err, &boundary) || boundary.ToolRunPauseStatus() != "waiting_external" {
			t.Fatalf("resource conflict did not release the worker through a typed pause: %v", err)
		}
	}
	if _, err := dispatch(runs[2], stage.Name(), "call-c"); err != nil || stage.calls != 1 {
		t.Fatalf("independent target did not execute: calls=%d err=%v", stage.calls, err)
	}
	if _, err := dispatch(runs[1], stage.Name(), "call-d"); err != nil || stage.calls != 2 {
		t.Fatalf("synchronously completed target stayed locked: calls=%d err=%v", stage.calls, err)
	}
	if _, err := dispatch(runs[0], prod.Name(), "call-a"); err == nil || !strings.Contains(err.Error(), "already claimed") || prod.calls != 1 {
		t.Fatalf("same effect was replayed: calls=%d err=%v", prod.calls, err)
	}
}

func TestExternalEffectMiddlewareClassifiesUnknownAndIsolatedCommands(t *testing.T) {
	read := toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin, ReadOnly: true, Category: "network"}
	if externalEffectPossible(map[string]interface{}{toolExecutionPolicyArg: read, "_tool_name": "web_search"}) {
		t.Fatal("built-in read-only web observation requested an effect claim")
	}
	unknown := toolExecutionPolicy{Origin: ToolSchemaOriginExternal, ReadOnly: true}
	if !externalEffectPossible(map[string]interface{}{toolExecutionPolicyArg: unknown, "_tool_name": "external_read"}) {
		t.Fatal("external tool metadata was trusted to bypass the conservative lane")
	}
	registry := NewRegistry()
	registry.Register(&externalClaimTestTool{newClaimTestTool("external_read", "deploy:production", true)})
	keys, complete := externalEffectTargets(map[string]interface{}{
		toolExecutionPolicyArg: unknown, "_tool_name": "external_read", "_registry": registry,
	}, nil)
	if len(keys) != 1 || keys[0] != control.UnknownExternalTarget || complete {
		t.Fatalf("external tool claimed a narrow resource through its own metadata: %v, %v", keys, complete)
	}
	localExec := map[string]interface{}{toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin},
		"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": false}
	if externalEffectPossible(localExec) {
		t.Fatal("confined local command requested an external claim")
	}
	localExec["_network_shared"] = true
	if !externalEffectPossible(localExec) {
		t.Fatal("shared-network command bypassed external claim")
	}
	localExec["_network_shared"] = false
	localExec["_effective_sandbox_mode"] = string(SandboxHost)
	if !externalEffectPossible(localExec) {
		t.Fatal("host command bypassed external claim")
	}
	localExec["command"] = "git status --short"
	if externalEffectPossible(localExec) {
		t.Fatal("proven observation could not inspect a held resource")
	}
}
