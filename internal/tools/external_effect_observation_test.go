package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// noStateObservations are proven read-only commands that read nothing outside
// their own literal arguments, the environment, or the clock, alone or as the
// only input of a stdin filter. None can show an effect's result.
var noStateObservations = []string{
	"true", ":", "cd .", "cd . && true", "echo published", "printf ok", "sleep 1", "date", "pwd",
	"test 1 = 1", "[ a = a ]", "which git", "echo done | cat", "echo x | tr a b", "seq 3 | sort",
	"echo '{}' | jq .",
	// Review round 3, item 2: a lone operator is a string test, jq -n reads
	// no input and the environment is not observed state, paste with no
	// operand reads its standard input, /dev/null holds nothing, and these
	// client commands report only the local client.
	"[ -f ]", "test -f", "[ -n -e ]", "test ! -d", "test '(' -e ')'", "[ a = a -a -n -f ]", "jq -n env", "jq -n '$ENV.HOME'", "jq --arg a b -n '$a'",
	"jq -nr '$ENV | keys'", "echo a | paste -s -d ,", "paste", "cat /dev/null", "head -c 4 /dev/null",
	"helm version", "helm env", "kubectl version --client", "kubectl --context prod version --client=true",
}

// stateObservations read a remote, a service, a repository, or a file.
var stateObservations = []string{
	"git ls-remote origin refs/tags/v1.2.0", "gh api repos/acme/app/releases/latest", "kubectl get deployment web",
	"cat release-state.txt", "test -f published.marker", "grep -q published deploy.log",
	"[ -f published.marker ]", "test ! -f stale.lock", "test release.tar -nt build.log", "[ -s out.log -a -r out.log ]", "test '(' -f out.log ')'", "[ -z x -o -f out.log ]",
	"jq .status state.json", "jq -n 'input' state.json", "jq -f filter.jq state.json", "yq .status state.yaml",
	"paste a.txt b.txt", "cat /dev/null state.txt", "kubectl version", "helm status web",
}

// Review round 2, item 1: a bound observation must read some state. A command
// whose every program reads none is refused before it runs, for the Run's own
// claim (no release) and for another Run's claim (no pass), while a real
// observation still binds.
func TestBoundObservationMustReadState(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		name := map[bool]string{false: "own claim", true: "another run's claim"}[foreign]
		t.Run(name, func(t *testing.T) {
			for _, command := range append(append([]string(nil), noStateObservations...), stateObservations...) {
				readsState := false
				for _, real := range stateObservations {
					readsState = readsState || real == command
				}
				t.Run(command, func(t *testing.T) {
					f := newEffectLifecycleFixture(t, 2)
					shell := failThenObserveShell()
					f.registry.Register(shell)
					owner, observer := f.runs[0], f.runs[0]
					if foreign {
						observer = f.runs[1]
					}
					if _, err := f.dispatch(owner, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
						t.Fatal("failing publish reported success")
					}
					claimID := f.heldClaimID(owner, "publish")
					before := len(shell.commands())
					result, err := f.dispatch(observer, "terminal", "observe", map[string]interface{}{"command": command, "effect_claim_id": claimID})
					if !readsState {
						var facts interface {
							ToolErrorCode() string
							ToolEffectState() string
						}
						if err == nil || !errors.As(err, &facts) || facts.ToolErrorCode() != "effect_observation_reads_no_state" ||
							facts.ToolEffectState() != "not_dispatched" || !strings.Contains(err.Error(), "state") {
							t.Fatalf("no-state observation was not refused: %+v %v", result, err)
						}
						if len(shell.commands()) != before {
							t.Fatal("refused observation still ran")
						}
						if left := f.unresolved(); len(left) != 1 || left[0].ID != claimID {
							t.Fatalf("no-state observation changed the claim: %+v", left)
						}
						if foreign {
							asks := 0
							f.setApproval(observer, ApprovalFullAuto, func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
								asks++
								return ToolApprovalDecision{Approved: true}, nil
							})
							if _, err := f.dispatch(observer, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil || asks != 1 {
								t.Fatalf("a refused no-state observation let the next call skip the person: asks=%d err=%v", asks, err)
							}
						}
						return
					}
					if err != nil {
						t.Fatalf("real observation did not bind: %+v %v", result, err)
					}
					want := "released"
					if foreign {
						want = "stays unresolved for its own run"
					}
					if !strings.Contains(result.Output, want) {
						t.Fatalf("real observation result: %q", result.Output)
					}
				})
			}
		})
	}
}

// The watcher form of a bound observation must read state too: a no-state
// check is refused before anything is registered or run.
func TestBoundWatcherObservationMustReadState(t *testing.T) {
	for _, command := range []string{"printf SUCCEEDED", "echo SUCCEEDED | cat"} {
		f := newEffectLifecycleFixture(t, 1)
		run := f.runs[0]
		person := "watch-no-state-" + run.ID
		cleanup := SetExecutionScope(person, ExecutionScope{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID,
			RunID: run.ID, TaskID: run.TaskID, WorkspaceID: "workspace", WorkspaceRoot: f.roots[run.ID], ParallelWork: true})
		_, err := NewExternalWatchTool(f.store).Execute(map[string]interface{}{"_tenant_id": person, "command": command,
			"cwd": f.roots[run.ID], "success_pattern": "^SUCCEEDED$", "effect_claim_id": "effectclaim_any"})
		cleanup()
		var facts interface{ ToolErrorCode() string }
		if err == nil || !errors.As(err, &facts) || facts.ToolErrorCode() != "effect_observation_reads_no_state" {
			t.Fatalf("%q: no-state watcher was not refused: %v", command, err)
		}
		watches, listErr := f.store.ListExternalWatchesForPerson(f.ctx, f.owner.TenantID, f.owner.PersonID, "all", 10, 0)
		if listErr != nil || len(watches) != 0 {
			t.Fatalf("%q: refused watcher was registered: %+v %v", command, watches, listErr)
		}
	}
	for _, command := range []string{"cat observed.state", "kubectl get deployment web -o jsonpath={.status}", "yq .status state.yaml"} {
		if !observationReadsState("watch_external", map[string]interface{}{"command": command}) {
			t.Fatalf("%q was not counted as reading state", command)
		}
	}
	for _, command := range []string{"yq e .status", "echo a: b | yq -o json ."} {
		if observationReadsState("watch_external", map[string]interface{}{"command": command}) {
			t.Fatalf("%q was counted as reading state", command)
		}
	}
}

// Review round 2, M1c: a bound observation counts only when its process facts
// prove success. A check can return no error and still prove nothing: a
// non-zero exit, no exit status, a process that never started, or no process
// facts at all. Such a check leaves the claim as it was, for the claim's own
// Run (no release) and for another Run (nothing to pass the claim with).
func TestBoundObservationNeedsProvenSuccess(t *testing.T) {
	zero := 0
	for _, check := range []struct {
		name   string
		result kernel.ToolDispatchResult
	}{
		{"non-zero exit", exitedResult(3)},
		{"no exit status", kernel.ToolDispatchResult{Output: "terminated", Process: &kernel.ToolProcessResult{Started: true}}},
		{"never started", kernel.ToolDispatchResult{Process: &kernel.ToolProcessResult{ExitCode: &zero}}},
		{"no process facts", kernel.ToolDispatchResult{Output: "abc refs/tags/v1.2.0"}},
	} {
		for _, foreign := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/foreign=%v", check.name, foreign), func(t *testing.T) {
				f := newEffectLifecycleFixture(t, 2)
				f.registry.Register(newLifecycleExecTool("terminal", func(command string) (kernel.ToolDispatchResult, error) {
					switch {
					case strings.HasPrefix(command, "npm publish"):
						return exitedResult(1), errors.New("command failed: exit status 1")
					case strings.HasPrefix(command, "git ls-remote"):
						return check.result, nil
					}
					return exitedResult(0), nil
				}))
				owner, observer := f.runs[0], f.runs[0]
				if foreign {
					observer = f.runs[1]
				}
				if _, err := f.dispatch(owner, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
					t.Fatal("failing publish reported success")
				}
				claimID := f.heldClaimID(owner, "publish")
				result, err := f.dispatch(observer, "terminal", "observe", map[string]interface{}{"command": "git ls-remote origin refs/tags/v1.2.0", "effect_claim_id": claimID})
				if err != nil || !strings.Contains(result.Output, "did not succeed") || strings.Contains(result.Output, "Bound this") {
					t.Fatalf("an unproven check counted as a successful observation: %+v %v", result, err)
				}
				if left := f.unresolved(); len(left) != 1 || left[0].ID != claimID || left[0].State != control.ExternalClaimUncertain {
					t.Fatalf("an unproven check changed the claim: %+v", left)
				}
				if !foreign {
					return
				}
				asks := 0
				f.setApproval(observer, ApprovalFullAuto, func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks++
					return ToolApprovalDecision{Approved: true}, nil
				})
				if _, err := f.dispatch(observer, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil || asks != 1 {
					t.Fatalf("an unproven check let the next call skip the person: asks=%d err=%v", asks, err)
				}
			})
		}
	}
}

// Review round 2, M1e: a check bound to a call still in flight is refused
// before it runs, since nothing can show that call's result yet: another
// Run's running call, or this Run's own running background process. After
// the call fails, the refused check has left the observer nothing to pass
// the claim with.
func TestBoundObservationOfACallInFlightDoesNotRun(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 2)
			shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil })
			f.registry.Register(shell)
			owner, observer := f.runs[0], f.runs[1]
			if foreign {
				inFlight(t, f, owner, "deploy")
			} else {
				observer = owner
				decision, err := f.store.ClaimExternalEffects(f.ctx, control.ExternalEffectClaimRequest{TenantID: f.owner.TenantID,
					PersonID: f.owner.PersonID, RunID: owner.ID, EffectID: "deploy", TargetKeys: []string{control.UnknownExternalTarget}})
				if err != nil || !decision.Granted {
					t.Fatalf("claim: %+v %v", decision, err)
				}
				if err := f.store.RecordExternalEffectDispatch(f.ctx, control.ExternalEffectDispatch{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID,
					RunID: owner.ID, EffectID: "deploy", ToolCallID: "deploy", ToolName: "terminal", Summary: "./serve.sh", Background: true}); err != nil {
					t.Fatal(err)
				}
				if err := f.store.MarkExternalEffectPossible(f.ctx, f.owner.TenantID, owner.ID, "deploy"); err != nil {
					t.Fatal(err)
				}
			}
			claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, owner.ID, "deploy")
			if err != nil || len(claims) != 1 {
				t.Fatalf("claims: %+v %v", claims, err)
			}
			result, err := f.dispatch(observer, "terminal", "observe", map[string]interface{}{"command": "git ls-remote origin", "effect_claim_id": claims[0].ID})
			var facts interface {
				ToolErrorCode() string
				ToolEffectState() string
			}
			if err == nil || !errors.As(err, &facts) || facts.ToolErrorCode() != "effect_claim_in_flight" || facts.ToolEffectState() != "not_dispatched" ||
				len(shell.commands()) != 0 {
				t.Fatalf("a check bound to a call in flight was not refused before it ran: %+v %v", result, err)
			}
			exit := 7
			if err := f.store.RecordExternalEffectReturn(f.ctx, control.ExternalEffectReturn{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID,
				RunID: owner.ID, EffectID: "deploy", ToolCallID: "deploy", Outcome: "exit 7", ExitCode: &exit}); err != nil {
				t.Fatal(err)
			}
			if left := f.unresolved(); len(left) != 1 || left[0].ID != claims[0].ID || left[0].Disposition != control.ExternalClaimLeftUncertain {
				t.Fatalf("claim after the refused check and the failed call: %+v", left)
			}
			if !foreign {
				return
			}
			asks := 0
			f.setApproval(observer, ApprovalFullAuto, func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
				asks++
				return ToolApprovalDecision{Approved: true}, nil
			})
			if _, err := f.dispatch(observer, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true}); err != nil || asks != 1 {
				t.Fatalf("a refused in-flight check let the next call skip the person: asks=%d err=%v", asks, err)
			}
		})
	}
}
