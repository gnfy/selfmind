package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// Review round 2, item 4: a Run parked on an effect of its own chain that a
// bound watcher still observes is told exactly that: no other Run's call is
// involved, and the watcher's success or the person's settlement releases the
// target. Review round 3, item 1: when another Run's left-uncertain effect
// also holds the target, the watcher's release does not resume the work by
// itself; the park says the person must then decide about that effect,
// naming it, instead of promising an automatic resume. A Run parked on
// another Run's in-flight call is still told so.
func TestParkOnOwnWatchedEffectDescribesItsOwnWatcher(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		continuation, beside, foreign bool
	}{
		{"own run", false, false, false},
		{"continuation of the run", true, false, false},
		{"own run beside another run's unresolved effect", false, true, false},
		{"another run's call", false, false, true},
		{"another run's call beside another run's unresolved effect", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 3)
			f.registry = NewRegistry()
			f.registry.UseResultMiddleware(externalEffectClaimMiddleware(f.store, 200*time.Millisecond))
			shell := newLifecycleExecTool("terminal", func(command string) (kernel.ToolDispatchResult, error) {
				if strings.HasPrefix(command, "npm publish") {
					return exitedResult(1), errors.New("command failed: exit status 1")
				}
				return exitedResult(0), nil
			})
			f.registry.Register(shell)
			owner, waiter := f.runs[0], f.runs[0]
			claimID, otherClaimID := "", ""
			if tc.foreign {
				waiter = f.runs[1]
				var tolerated []string
				if tc.beside {
					if _, err := f.dispatch(f.runs[2], "terminal", "publish-next", map[string]interface{}{"command": "npm publish --tag next", "_network_shared": true}); err == nil {
						t.Fatal("failing publish reported success")
					}
					otherClaimID = f.heldClaimID(f.runs[2], "publish-next")
					tolerated = []string{otherClaimID}
				}
				inFlight(t, f, owner, "deploy", tolerated...)
			} else {
				if tc.beside {
					if _, err := f.dispatch(f.runs[1], "terminal", "publish-next", map[string]interface{}{"command": "npm publish --tag next", "_network_shared": true}); err == nil {
						t.Fatal("failing publish reported success")
					}
					otherClaimID = f.heldClaimID(f.runs[1], "publish-next")
					// The person lets this Run's own publish pass the other
					// Run's unresolved effect once.
					f.setApproval(owner, ApprovalFullAuto, func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
						return ToolApprovalDecision{Approved: true}, nil
					})
				}
				if _, err := f.dispatch(owner, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
					t.Fatal("failing publish reported success")
				}
				claimID = f.heldClaimID(owner, "publish")
				if _, err := f.store.CreateExternalWatch(f.ctx, control.ExternalWatch{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID,
					TaskID: owner.TaskID, RunID: owner.ID, Channel: "cli", CWD: f.roots[owner.ID], Command: "npm view app version",
					SuccessPattern: "1.2.0", PreflightReceipt: control.ExternalWatchPreflightReceipt{
						Version: control.ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: claimID}}); err != nil {
					t.Fatal(err)
				}
				if tc.continuation {
					waiter = f.continueRun(owner)
				}
			}
			_, err := f.dispatch(waiter, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true})
			var pause interface {
				ToolRunPause() (string, string, bool)
				ToolRunPauseStatus() string
				ToolRecoveryHint() string
			}
			if err == nil || !errors.As(err, &pause) || pause.ToolRunPauseStatus() != "waiting_external" {
				t.Fatalf("the conflicting call did not park: %v", err)
			}
			_, message, _ := pause.ToolRunPause()
			told := err.Error() + "\n" + message + "\n" + pause.ToolRecoveryHint()
			if tc.foreign {
				if !strings.Contains(told, "another run") || !strings.Contains(err.Error(), owner.ID) {
					t.Fatalf("a park on another run's call does not say so: %s", told)
				}
				if !tc.beside {
					return
				}
				for _, w := range []string{otherClaimID, "/effects settle " + otherClaimID, "your decision"} {
					if !strings.Contains(told, w) {
						t.Fatalf("a park beside another run's unresolved effect does not mention %q: %s", w, told)
					}
				}
				if strings.Contains(told, "SelfMind resumes this work once") {
					t.Fatalf("a park beside another run's unresolved effect promises an automatic resume: %s", told)
				}
				return
			}
			want := []string{claimID, "watcher", "/effects settle " + claimID}
			wrong := []string{"another run", "in-flight", "in flight"}
			if tc.beside {
				// The other Run's effect needs the person after the watcher.
				want = []string{claimID, "watcher", otherClaimID, "/effects settle " + otherClaimID, "your decision"}
				wrong = []string{"SelfMind resumes this work once", "another run's call", "in-flight", "in flight"}
			}
			for _, w := range want {
				if !strings.Contains(told, w) {
					t.Fatalf("a park on this work's own watched effect does not mention %q: %s", w, told)
				}
			}
			for _, w := range wrong {
				if strings.Contains(told, w) {
					t.Fatalf("a park on this work's own watched effect says %q: %s", w, told)
				}
			}
		})
	}
}

// continueRun ends run as blocked and starts its exact continuation with the
// same execution scope shape.
func (f *effectLifecycleFixture) continueRun(run *control.Run) *control.Run {
	f.t.Helper()
	if err := f.store.FinishRun(f.ctx, f.owner.TenantID, run.ID, "blocked"); err != nil {
		f.t.Fatal(err)
	}
	task, err := f.store.GetTask(f.ctx, f.owner.TenantID, run.TaskID)
	if err != nil {
		f.t.Fatal(err)
	}
	child, err := f.store.StartRunWithOptions(f.ctx, task, "cli", "continue", control.StartRunOptions{MaxActiveRuns: 3, ResumesRunID: run.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	root := f.roots[run.ID]
	f.roots[child.ID] = root
	cleanup := SetExecutionScope(ExecutionScopeKeyForRun(child.ID), ExecutionScope{TenantID: f.owner.TenantID,
		PersonID: f.owner.PersonID, RunID: child.ID, TaskID: child.TaskID, WorkspaceID: "workspace", WorkspaceRoot: root,
		AllowedRoots: []string{root}, ParallelWork: true})
	f.t.Cleanup(cleanup)
	return child
}
