package tools

import (
	"context"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
	"strings"
	"testing"
)

func TestExternalBlockerPrecheckAvoidsAskAndRechecksAfterApproval(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "known blocker", true: "changed during approval"}[race], func(t *testing.T) {
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
			runs := make([]*control.Run, 2)
			for i, title := range []string{"earlier", "current"} {
				task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: title, Channel: title})
				if err != nil {
					t.Fatal(err)
				}
				runs[i], err = store.StartRunWithOptions(ctx, task, title, title, control.StartRunOptions{MaxActiveRuns: 2})
				if err != nil {
					t.Fatal(err)
				}
			}
			block := func() {
				t.Helper()
				decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: runs[0].ID, EffectID: "earlier-effect", TargetKeys: []string{"target:one"}})
				if err != nil || !decision.Granted {
					t.Fatalf("claim: %+v %v", decision, err)
				}
				if err = store.MarkExternalEffectPossible(ctx, owner.TenantID, runs[0].ID, "earlier-effect"); err != nil {
					t.Fatal(err)
				}
				if err = store.FinishRun(ctx, owner.TenantID, runs[0].ID, "blocked"); err != nil {
					t.Fatal(err)
				}
			}
			if !race {
				block()
			}
			asks := 0
			scope := ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: runs[1].ID, WorkspaceID: "workspace", WorkspaceRoot: t.TempDir(), ParallelWork: true, ApprovalMode: ApprovalReadOnly,
				Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks++
					if race {
						block()
					}
					return ToolApprovalDecision{Approved: true}, nil
				}}
			cleanup := SetExecutionScope(ExecutionScopeKeyForRun(runs[1].ID), scope)
			defer cleanup()
			registry := NewRegistry()
			registry.UseMiddleware(SmartApprovalMiddleware(scope.WorkspaceRoot, ExternalEffectPrecheck(store)))
			registry.UseResultMiddleware(ExternalEffectClaimMiddleware(store))
			tool := newClaimTestTool("write_file", "target:one", true)
			registry.Register(tool)
			result, err := registry.DispatchResult(tool.Name(), map[string]interface{}{"_tool_call_id": "attempt", "_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(runs[1].ID)}})
			if err == nil || !strings.Contains(err.Error(), "unresolved") || tool.calls != 0 || result.Invoked == nil || *result.Invoked {
				t.Fatalf("blocked effect dispatched: calls=%d result=%+v err=%v", tool.calls, result, err)
			}
			wantAsks := 0
			if race {
				wantAsks = 1
			}
			if asks != wantAsks {
				t.Fatalf("asks=%d want=%d", asks, wantAsks)
			}
			claims, needs, err := store.InspectExternalEffectBlockers(ctx, owner.TenantID, owner.PersonID, runs[1].ID, []string{"target:one"})
			if err != nil || !needs || len(claims) != 1 || claims[0].RunID != runs[0].ID {
				t.Fatalf("precheck changed held effect: %+v %v", claims, err)
			}
			if !race {
				independent := newClaimTestTool("edit_file", "target:two", true)
				registry.Register(independent)
				_, err = registry.Dispatch(independent.Name(), map[string]interface{}{"_tool_call_id": "different-target", "_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(runs[1].ID)}})
				if err != nil || independent.calls != 1 || asks != 1 {
					t.Fatalf("independent target blocked: asks=%d calls=%d err=%v", asks, independent.calls, err)
				}
			}
		})
	}
}

func TestExternalPrecheckPrecedesCredentialCapabilityAndAllowsObservations(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "effects", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.StartRun(ctx, task, "cli", "old")
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: old.ID, EffectID: "unknown", TargetKeys: []string{control.UnknownExternalTarget}}); err != nil || !decision.Granted {
		t.Fatalf("claim=%+v err=%v", decision, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, old.ID, "unknown"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, old.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "current")
	if err != nil {
		t.Fatal(err)
	}
	asks, calls := 0, 0
	root := t.TempDir()
	cleanup := SetExecutionScope("precheck-credentials", ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, WorkspaceID: "workspace", WorkspaceRoot: root, AllowedRoots: []string{root}, TrustLevel: executionenv.TrustUntrusted, ParallelWork: true, ApprovalMode: ApprovalSmart,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			return ToolApprovalDecision{Approved: true, Scope: "run"}, nil
		},
	})
	defer cleanup()
	executor := ExecutionCapabilityMiddleware(ExternalEffectPrecheck(store))(func(map[string]interface{}) (string, error) { calls++; return "observed", nil })
	for _, command := range []string{"aws s3 rm s3://example/object", "gcloud builds submit .", "kubectl delete deployment example", "curl -X POST https://example.test"} {
		args := map[string]interface{}{"_tenant_id": "precheck-credentials", "_tool_name": "terminal", "command": command, toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin}}
		if _, err := executor(args); err == nil || !strings.Contains(err.Error(), "unresolved") {
			t.Fatalf("%s missed known blocker: %v", command, err)
		}
		if asks != 0 || calls != 0 || args["_network_shared"] == true {
			t.Fatalf("blocked projection granted or asked: asks=%d calls=%d args=%+v", asks, calls, args)
		}
	}
	args := map[string]interface{}{"_tenant_id": "precheck-credentials", "_tool_name": "terminal", "command": "aws sts get-caller-identity", toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin}}
	if _, err := executor(args); err != nil || calls != 1 || asks != 1 {
		t.Fatalf("proven observation was blocked: calls=%d asks=%d err=%v", calls, asks, err)
	}
}
