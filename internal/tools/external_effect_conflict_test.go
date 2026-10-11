package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// failThenObserveShell fails the first effectful command it sees and lets
// every other command (including read-only checks) exit 0.
func failThenObserveShell() *lifecycleExecTool {
	failed := false
	var mu sync.Mutex
	return newLifecycleExecTool("terminal", func(command string) (kernel.ToolDispatchResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if !failed && !strings.HasPrefix(command, "git ls-remote") && !strings.HasPrefix(command, "cat ") {
			failed = true
			return exitedResult(1), errors.New("command failed: exit status 1")
		}
		return exitedResult(0), nil
	})
}

func (f *effectLifecycleFixture) setApproval(run *control.Run, mode ApprovalMode, approval ToolApprovalHandler) {
	f.t.Helper()
	root := f.roots[run.ID]
	cleanup := SetExecutionScope(ExecutionScopeKeyForRun(run.ID), ExecutionScope{TenantID: f.owner.TenantID,
		PersonID: f.owner.PersonID, RunID: run.ID, TaskID: run.TaskID, WorkspaceID: "workspace", WorkspaceRoot: root,
		AllowedRoots: []string{root}, ParallelWork: true, ApprovalMode: mode, Approval: approval})
	f.t.Cleanup(cleanup)
}

func (f *effectLifecycleFixture) heldClaimID(run *control.Run, call string) string {
	f.t.Helper()
	claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, run.ID, kernel.ToolEffectID(run.ID, call))
	if err != nil || len(claims) == 0 {
		f.t.Fatalf("claims for %s: %+v %v", call, claims, err)
	}
	return claims[0].ID
}

// Item 3: the same chain's next conflicting effect waits for a successful
// read-only observation the model binds to the claim, made after the claim.
// That observation releases the claim with a ref naming the observing call.
func TestBoundReadOnlyObservationReleasesOwnClaim(t *testing.T) {
	for _, observe := range []string{"git ls-remote origin refs/tags/v1.2.0", "cat release-state.txt"} {
		t.Run(observe, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			shell := failThenObserveShell()
			f.registry.Register(shell)
			run := f.runs[0]
			if _, err := f.dispatch(run, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
				t.Fatal("failing publish reported success")
			}
			claimID := f.heldClaimID(run, "publish")
			if _, err := f.dispatch(run, "terminal", "push", map[string]interface{}{"command": "git push origin v1.2.0", "_network_shared": true}); err == nil ||
				!strings.Contains(err.Error(), claimID) || !strings.Contains(err.Error(), "`npm publish`") {
				t.Fatalf("own unresolved effect did not refuse with the claim: %v", err)
			}
			result, err := f.dispatch(run, "terminal", "observe", map[string]interface{}{"command": observe, "effect_claim_id": claimID, "_network_shared": true})
			if err != nil || !strings.Contains(result.Output, "released") {
				t.Fatalf("bound observation: %+v %v", result, err)
			}
			claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, run.ID, kernel.ToolEffectID(run.ID, "publish"))
			if err != nil || claims[0].State != control.ExternalClaimObserved || claims[0].ObservationRef != "observation:"+run.ID+":observe" {
				t.Fatalf("observation did not release with a ref naming the call: %+v %v", claims, err)
			}
			if _, err := f.dispatch(run, "terminal", "push-again", map[string]interface{}{"command": "git push origin v1.2.0", "_network_shared": true}); err != nil {
				t.Fatalf("released effect still blocked the chain: %v", err)
			}
		})
	}
}

// What the runtime checks before an observation binds: a proven read-only
// command, the person's own unobserved claim, a call that has returned, and
// a successful check. The model's wording decides none of these.
func TestBoundObservationChecksProofIdentityAndSuccess(t *testing.T) {
	f := newEffectLifecycleFixture(t, 2)
	shell := newLifecycleExecTool("terminal", func(command string) (kernel.ToolDispatchResult, error) {
		if strings.Contains(command, "missing-ref") {
			return exitedResult(2), errors.New("command failed: exit status 2")
		}
		if strings.HasPrefix(command, "deploy") {
			return exitedResult(1), errors.New("command failed: exit status 1")
		}
		return exitedResult(0), nil
	})
	f.registry.Register(shell)
	run := f.runs[0]
	if _, err := f.dispatch(run, "terminal", "deploy", map[string]interface{}{"command": "deploy --target prod", "_network_shared": true}); err == nil {
		t.Fatal("failing deploy reported success")
	}
	claimID := f.heldClaimID(run, "deploy")
	cases := []struct {
		name, command, claim, want string
	}{
		{"not read-only", "deploy --target prod --check", claimID, "proven read-only observation"},
		{"unknown claim", "git ls-remote origin", "effectclaim_missing", "No unresolved external effect claim"},
		{"failed check", "git ls-remote origin missing-ref", claimID, "stays unresolved"},
	}
	for _, tc := range cases {
		before := len(shell.commands())
		result, err := f.dispatch(run, "terminal", "check-"+strings.ReplaceAll(tc.name, " ", "-"), map[string]interface{}{"command": tc.command, "effect_claim_id": tc.claim})
		message := result.Output
		if err != nil {
			message += err.Error()
		}
		if !strings.Contains(message, tc.want) {
			t.Fatalf("%s: %q", tc.name, message)
		}
		if tc.name != "failed check" && len(shell.commands()) != before {
			t.Fatalf("%s: refused binding still ran the command", tc.name)
		}
	}
	if left := f.unresolved(); len(left) != 1 || left[0].ID != claimID {
		t.Fatalf("an invalid observation released the claim: %+v", left)
	}
}

// Item 4: another Run's left-uncertain effect turns a full-auto Run's
// conflicting call into one human decision at the claim. Approval passes only
// this call and stores nothing; the next conflicting call asks again, and a
// rejection stops the call for good. The other claim stays uncertain.
func TestForeignUncertainEffectAsksOncePerCallInFullAuto(t *testing.T) {
	for _, mode := range []ApprovalMode{ApprovalFullAuto, ApprovalSmart} {
		t.Run(string(mode), func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 2)
			shell := failThenObserveShell()
			f.registry.Register(shell)
			owner, waiter := f.runs[0], f.runs[1]
			if _, err := f.dispatch(owner, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
				t.Fatal("failing publish reported success")
			}
			claimID := f.heldClaimID(owner, "publish")
			var requests []ToolApprovalRequest
			answer := true
			f.setApproval(waiter, mode, func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
				requests = append(requests, req)
				return ToolApprovalDecision{Approved: answer, Reason: map[bool]string{false: "not until the publish is checked"}[answer]}, nil
			})
			for i := 0; i < 2; i++ {
				if _, err := f.dispatch(waiter, "terminal", "push-"+string(rune('a'+i)), map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil {
					t.Fatalf("approved call %d: %v", i, err)
				}
			}
			if len(requests) != 2 {
				t.Fatalf("asks=%d, want one per conflicting call", len(requests))
			}
			for _, req := range requests {
				if req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly || req.GrantClass != "" || len(req.RuleCandidates) != 0 ||
					!strings.Contains(req.Reason, claimID) || !strings.Contains(req.Reason, owner.ID) || !strings.Contains(req.Reason, "`npm publish`") {
					t.Fatalf("ask did not name the claim as a once-only decision: %+v", req)
				}
			}
			answer = false
			before := len(shell.commands())
			_, err := f.dispatch(waiter, "terminal", "push-c", map[string]interface{}{"command": "git push origin main", "_network_shared": true})
			if err == nil || !strings.HasPrefix(err.Error(), "operation rejected") || len(shell.commands()) != before || len(requests) != 3 {
				t.Fatalf("rejection did not stop the call once: asks=%d err=%v", len(requests), err)
			}
			if left := f.unresolved(); len(left) != 1 || left[0].ID != claimID || left[0].State != control.ExternalClaimUncertain {
				t.Fatalf("approval touched the other Run's claim: %+v", left)
			}
		})
	}
}

// D7: instead of asking, this Run may first observe the other Run's effect
// read-only and bind the check to its claim. The next conflicting call then
// passes once without a question; the other claim is never settled by it.
func TestOwnObservationOfForeignClaimPassesOnceWithoutAsking(t *testing.T) {
	f := newEffectLifecycleFixture(t, 2)
	shell := failThenObserveShell()
	f.registry.Register(shell)
	owner, waiter := f.runs[0], f.runs[1]
	if _, err := f.dispatch(owner, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
		t.Fatal("failing publish reported success")
	}
	claimID := f.heldClaimID(owner, "publish")
	asks := 0
	f.setApproval(waiter, ApprovalFullAuto, func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true}, nil
	})
	result, err := f.dispatch(waiter, "terminal", "observe", map[string]interface{}{"command": "git ls-remote origin", "effect_claim_id": claimID})
	if err != nil || !strings.Contains(result.Output, "stays unresolved for its own run") {
		t.Fatalf("observation of another Run's claim: %+v %v", result, err)
	}
	if _, err := f.dispatch(waiter, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil || asks != 0 {
		t.Fatalf("observed claim still asked or refused: asks=%d err=%v", asks, err)
	}
	if left := f.unresolved(); len(left) != 1 || left[0].ID != claimID {
		t.Fatalf("an observation by another Run settled the claim: %+v", left)
	}
	if _, err := f.dispatch(waiter, "terminal", "push-again", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil || asks != 1 {
		t.Fatalf("a spent observation passed a second call: asks=%d err=%v", asks, err)
	}
}

// Item 5: another Run's in-flight call is waited for in place, and this call
// dispatches as soon as that call settles; no durable wait is written.
func TestInFlightForeignEffectIsWaitedForInPlace(t *testing.T) {
	for _, settle := range []string{"observed", "settled by the owner's success"} {
		t.Run(settle, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 2)
			f.registry = NewRegistry()
			f.registry.UseResultMiddleware(externalEffectClaimMiddleware(f.store, 10*time.Second))
			shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil })
			f.registry.Register(shell)
			owner, waiter := f.runs[0], f.runs[1]
			inFlight(t, f, owner, "deploy")
			go func() {
				time.Sleep(400 * time.Millisecond)
				_ = f.store.ObserveExternalEffectGroup(context.Background(), f.owner.TenantID, f.owner.PersonID, owner.ID, "deploy", "tool:completed:deploy")
			}()
			start := time.Now()
			if _, err := f.dispatch(waiter, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil {
				t.Fatalf("waiter did not dispatch after the claim settled: %v", err)
			}
			if waited := time.Since(start); waited < 300*time.Millisecond || waited > 8*time.Second {
				t.Fatalf("waited %s", waited)
			}
			if waits, err := f.store.ListExternalResourceWaits(f.ctx, f.owner.TenantID, f.owner.PersonID, 10); err != nil || len(waits) != 0 {
				t.Fatalf("an in-place wait wrote a durable wait: %+v %v", waits, err)
			}
		})
	}
}

// Past the grace the waiting Run parks exactly as before; a cancelled wait
// ends at once without dispatching.
func TestInFlightWaitParksAfterGraceAndHonorsCancellation(t *testing.T) {
	f := newEffectLifecycleFixture(t, 3)
	f.registry = NewRegistry()
	f.registry.UseResultMiddleware(externalEffectClaimMiddleware(f.store, 500*time.Millisecond))
	shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil })
	f.registry.Register(shell)
	owner := f.runs[0]
	inFlight(t, f, owner, "deploy")
	_, err := f.dispatch(f.runs[1], "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true})
	var pause interface{ ToolRunPauseStatus() string }
	if err == nil || !errors.As(err, &pause) || pause.ToolRunPauseStatus() != "waiting_external" || len(shell.commands()) != 0 {
		t.Fatalf("grace expiry did not park: %v", err)
	}
	if !strings.Contains(err.Error(), owner.ID) {
		t.Fatalf("park does not name the owner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	f.registry = NewRegistry()
	f.registry.UseResultMiddleware(externalEffectClaimMiddleware(f.store, time.Minute))
	f.registry.Register(shell)
	start := time.Now()
	result, err := f.dispatch(f.runs[2], "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true, "_context": ctx})
	if err == nil || time.Since(start) > 5*time.Second || len(shell.commands()) != 0 || result.Invoked == nil || *result.Invoked {
		t.Fatalf("cancelled wait: %+v %v after %s", result, err, time.Since(start))
	}
}

// An in-flight call that then fails leaves its claim uncertain; the waiting
// call stops waiting and needs the person's one-time decision instead.
func TestInFlightWaitTurnsIntoApprovalWhenTheCallFails(t *testing.T) {
	f := newEffectLifecycleFixture(t, 2)
	f.registry = NewRegistry()
	f.registry.UseResultMiddleware(externalEffectClaimMiddleware(f.store, 10*time.Second))
	shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil })
	f.registry.Register(shell)
	owner, waiter := f.runs[0], f.runs[1]
	inFlight(t, f, owner, "deploy")
	asks := 0
	f.setApproval(waiter, ApprovalFullAuto, func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true}, nil
	})
	go func() {
		time.Sleep(300 * time.Millisecond)
		exit := 7
		_ = f.store.RecordExternalEffectReturn(context.Background(), control.ExternalEffectReturn{TenantID: f.owner.TenantID,
			PersonID: f.owner.PersonID, RunID: owner.ID, EffectID: "deploy", ToolCallID: "deploy", Outcome: "exit 7", ExitCode: &exit})
	}()
	if _, err := f.dispatch(waiter, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil || asks != 1 {
		t.Fatalf("waiter after the owner's failure: asks=%d err=%v", asks, err)
	}
}

// inFlight dispatches an effect for run without returning it. tolerated names
// other Runs' left-uncertain claims the person let this effect pass once.
func inFlight(t *testing.T, f *effectLifecycleFixture, run *control.Run, effectID string, tolerated ...string) {
	t.Helper()
	decision, err := f.store.ClaimExternalEffects(f.ctx, control.ExternalEffectClaimRequest{TenantID: f.owner.TenantID,
		PersonID: f.owner.PersonID, RunID: run.ID, EffectID: effectID, TargetKeys: []string{control.UnknownExternalTarget},
		ToleratedClaimIDs: tolerated})
	if err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	if err := f.store.RecordExternalEffectDispatch(f.ctx, control.ExternalEffectDispatch{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: effectID, ToolName: "terminal", Summary: "deploy --target prod"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkExternalEffectPossible(f.ctx, f.owner.TenantID, run.ID, effectID); err != nil {
		t.Fatal(err)
	}
}
