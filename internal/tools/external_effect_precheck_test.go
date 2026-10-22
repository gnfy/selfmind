package tools

import (
	"context"
	"errors"
	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
	"strings"
	"sync"
	"testing"
)

// leaveUncertainEffect gives run a dispatched effect whose call returned
// without proving success, then parks the run as blocked.
func leaveUncertainEffect(t *testing.T, store *control.Store, owner *control.IdentityContext, run *control.Run, effectID string, targets ...string) control.ExternalEffectClaim {
	t.Helper()
	ctx := context.Background()
	decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, TargetKeys: targets})
	if err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	if err := store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: "call-" + effectID, ToolName: "terminal", Summary: "release --publish " + effectID}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, effectID); err != nil {
		t.Fatal(err)
	}
	exit := 1
	if err := store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: "call-" + effectID, Outcome: "exit 1", ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	return decision.Claims[0]
}

// The Run's own chain is refused before any approval is asked: approval
// cannot establish the earlier effect's result. The refusal names the claim
// and the observation forms that release it.
func TestOwnChainUnresolvedEffectIsRefusedBeforeApproval(t *testing.T) {
	for _, target := range []string{"target:one", control.UnknownExternalTarget} {
		t.Run(target, func(t *testing.T) {
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
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "release", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			earlier, err := store.StartRunWithOptions(ctx, task, "cli", "publish", control.StartRunOptions{MaxActiveRuns: 2})
			if err != nil {
				t.Fatal(err)
			}
			held := leaveUncertainEffect(t, store, owner, earlier, "publish", "target:one")
			continuation, err := store.StartRunWithOptions(ctx, task, "cli", "continue", control.StartRunOptions{MaxActiveRuns: 2, ResumesRunID: earlier.ID})
			if err != nil {
				t.Fatal(err)
			}
			asks := 0
			scope := ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: continuation.ID, WorkspaceID: "workspace",
				WorkspaceRoot: t.TempDir(), ParallelWork: true, ApprovalMode: ApprovalReadOnly,
				Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks++
					return ToolApprovalDecision{Approved: true}, nil
				}}
			cleanup := SetExecutionScope(ExecutionScopeKeyForRun(continuation.ID), scope)
			defer cleanup()
			registry := NewRegistry()
			registry.UseMiddleware(SmartApprovalMiddleware(scope.WorkspaceRoot, ExternalEffectPrecheck(store)))
			registry.UseResultMiddleware(ExternalEffectClaimMiddleware(store))
			tool := newClaimTestTool("write_file", target, true)
			registry.Register(tool)
			result, err := registry.DispatchResult(tool.Name(), map[string]interface{}{"_tool_call_id": "attempt",
				"_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(continuation.ID)}})
			if err == nil || tool.calls != 0 || asks != 0 || result.Invoked == nil || *result.Invoked {
				t.Fatalf("own unresolved effect dispatched or asked: calls=%d asks=%d result=%+v err=%v", tool.calls, asks, result, err)
			}
			for _, want := range []string{held.ID, "release --publish publish", "exit 1", `effect_claim_id="` + held.ID + `"`, "watch_external", "/effects settle " + held.ID} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal does not say %q: %v", want, err)
				}
			}
			var facts interface{ ToolErrorCode() string }
			if !errors.As(err, &facts) || facts.ToolErrorCode() != "external_effect_unresolved" {
				t.Fatalf("refusal code: %v", err)
			}
		})
	}
}

// Another Run's left-uncertain effect turns this Run's conflicting call into
// one human decision. When the mode already asks, the decision joins that ask;
// a claim that appears during the ask gets its own ask at the claim.
// Approval passes only this call; the other claim stays unresolved.
type effectPrecheckJudge struct {
	fakeJudge
	onJudge func()
}

func (j *effectPrecheckJudge) Judge(ctx context.Context, prompt string) (string, error) {
	reply, err := j.fakeJudge.Judge(ctx, prompt)
	if j.onJudge != nil {
		j.onJudge()
	}
	return reply, err
}

func TestForeignUncertainEffectIsOneTimeApproval(t *testing.T) {
	for _, tc := range []struct {
		mode ApprovalMode
		race bool
		deny bool
	}{
		{ApprovalReadOnly, false, false}, {ApprovalReadOnly, true, false},
		{ApprovalSmart, false, false}, {ApprovalSmart, true, false},
		{ApprovalFullAuto, false, false}, {ApprovalFullAuto, true, false},
		{ApprovalReadOnly, false, true}, {ApprovalSmart, false, true}, {ApprovalFullAuto, false, true},
	} {
		t.Run(string(tc.mode)+"/"+map[bool]string{false: "known before the ask", true: "appears during approval"}[tc.race]+map[bool]string{false: "", true: "/rejected"}[tc.deny], func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
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
			var held control.ExternalEffectClaim
			var once sync.Once
			block := func() {
				once.Do(func() { held = leaveUncertainEffect(t, store, owner, runs[0], "earlier-effect", "target:one") })
			}
			if !tc.race {
				block()
			}
			var reasons []string
			judge := &effectPrecheckJudge{fakeJudge: fakeJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"The user requested this bounded state write."}`}}
			scope := ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: runs[1].ID, WorkspaceID: "workspace", WorkspaceRoot: t.TempDir(), ParallelWork: true, ApprovalMode: tc.mode, Judge: judge,
				IntentSnapshot: func() RunIntentSnapshot {
					return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "write the requested state"}
				},
				Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					reasons = append(reasons, req.Reason)
					if strings.Contains(req.Reason, "another run left") && req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly {
						t.Fatalf("an ask about another run's claim offered reusable authority: %+v", req)
					}
					block()
					return ToolApprovalDecision{Approved: !tc.deny}, nil
				}}
			// For the race, the conflicting claim appears after the ordinary
			// smart/full-auto approval boundary has passed.
			if tc.race && tc.mode != ApprovalReadOnly {
				judge.onJudge = block
			}
			cleanup := SetExecutionScope(ExecutionScopeKeyForRun(runs[1].ID), scope)
			defer cleanup()
			registry := NewRegistry()
			registry.UseMiddleware(SmartApprovalMiddleware(scope.WorkspaceRoot, ExternalEffectPrecheck(store)))
			if tc.race && tc.mode == ApprovalFullAuto {
				registry.UseMiddleware(func(next ToolExecutor) ToolExecutor {
					return func(args map[string]interface{}) (string, error) { block(); return next(args) }
				})
			}
			registry.UseResultMiddleware(ExternalEffectClaimMiddleware(store))
			tool := newClaimTestTool("write_file", "target:one", true)
			registry.Register(tool)
			_, dispatchErr := registry.DispatchResult(tool.Name(), map[string]interface{}{"_tool_call_id": "attempt", "_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(runs[1].ID)}})
			if tc.deny {
				if dispatchErr == nil || !strings.Contains(dispatchErr.Error(), "operation rejected") || tool.calls != 0 || len(reasons) != 1 || judge.calls != 0 {
					t.Fatalf("human refusal was bypassed: calls=%d asks=%d judges=%d err=%v", tool.calls, len(reasons), judge.calls, dispatchErr)
				}
				return
			}
			if err := dispatchErr; err != nil || tool.calls != 1 {
				t.Fatalf("approved call did not dispatch once: calls=%d err=%v", tool.calls, err)
			}
			wantAsks := 1
			if tc.race && tc.mode == ApprovalReadOnly {
				wantAsks = 2
			}
			if len(reasons) != wantAsks {
				t.Fatalf("asks=%d want %d: %q", len(reasons), wantAsks, reasons)
			}
			wantJudges := 0
			if tc.race && tc.mode == ApprovalSmart {
				wantJudges = 1
			}
			if judge.calls != wantJudges {
				t.Fatalf("judge calls=%d, want %d; a known mandatory human decision must not be judged first", judge.calls, wantJudges)
			}
			last := reasons[len(reasons)-1]
			if !strings.Contains(last, "target scope is known") || !strings.Contains(last, "/effects settle "+held.ID) {
				t.Fatalf("approval does not explain scope and the existing recovery control: %q", last)
			}
			for _, want := range []string{held.ID, runs[0].ID, "release --publish earlier-effect", "exit 1", "only this call"} {
				if !strings.Contains(last, want) {
					t.Fatalf("approval does not name %q: %q", want, last)
				}
			}
			claims, needs, err := store.InspectExternalEffectBlockers(ctx, owner.TenantID, owner.PersonID, runs[1].ID, []string{"target:one"})
			if err != nil || !needs || len(claims) != 1 || claims[0].ID != held.ID || claims[0].State != control.ExternalClaimUncertain {
				t.Fatalf("approval settled or hid the other claim: %+v %v", claims, err)
			}
			independent := newClaimTestTool("edit_file", "target:two", true)
			registry.Register(independent)
			asksBefore := len(reasons)
			wantIndependentAsks := asksBefore
			if tc.mode == ApprovalReadOnly {
				wantIndependentAsks++
			}
			if _, err = registry.Dispatch(independent.Name(), map[string]interface{}{"_tool_call_id": "different-target", "_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(runs[1].ID)}}); err != nil || independent.calls != 1 || len(reasons) != wantIndependentAsks {
				t.Fatalf("independent target: asks=%d calls=%d err=%v", len(reasons), independent.calls, err)
			}
			if len(reasons) > asksBefore && strings.Contains(reasons[len(reasons)-1], held.ID) {
				t.Fatalf("an independent target was asked about the other claim: %q", reasons[len(reasons)-1])
			}
		})
	}
}

func TestForeignEffectApprovalExplainsUnknownTargetScope(t *testing.T) {
	claims := []control.ExternalEffectClaim{
		{ID: "named", RunID: "earlier", EffectID: "same-effect", TargetKey: "target:a", Summary: "prepare state"},
		{ID: "unknown", RunID: "earlier", EffectID: "same-effect", TargetKey: control.UnknownExternalTarget},
	}
	reason := foreignEffectApprovalReason(claims)
	for _, want := range []string{"unknown target scope", "cannot prove", "independent", "only this call", "/effects settle named"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("unknown-scope diagnostic missing %q: %s", want, reason)
		}
	}
	if strings.Contains(reason, "target scope is known") {
		t.Fatal("deduplicating an effect hid one of its unknown targets")
	}
	var asked string
	scope := ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: "current", Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asked = req.Reason
		return ToolApprovalDecision{Approved: false}, nil
	}}
	err := approveForeignExternalEffects(context.Background(), scope, map[string]interface{}{"_tool_name": "write_file"}, claims)
	if err == nil || !strings.Contains(asked, "unknown target scope") {
		t.Fatalf("dispatch boundary lost the unknown row before asking: reason=%q err=%v", asked, err)
	}
}

// The credential-capability ask waits on the precheck: a call its own chain
// cannot dispatch asks for nothing, while proven observations still run.
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
	leaveUncertainEffect(t, store, owner, old, "unknown", control.UnknownExternalTarget)
	run, err := store.StartRunWithOptions(ctx, task, "cli", "current", control.StartRunOptions{ResumesRunID: old.ID})
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
	for i, command := range []string{"aws sts get-caller-identity", "unset HTTPS_PROXY HTTP_PROXY ALL_PROXY; gh pr view 19 --json state", "env -u https_proxy gcloud builds describe build-2"} {
		args := map[string]interface{}{"_tenant_id": "precheck-credentials", "_tool_name": "terminal", "command": command, toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin}}
		if _, err := executor(args); err != nil || calls != i+1 || asks != i+1 {
			t.Fatalf("proven observation was blocked: calls=%d asks=%d err=%v", calls, asks, err)
		}
	}
}
