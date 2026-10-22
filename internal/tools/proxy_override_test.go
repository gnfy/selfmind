package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"selfmind/internal/executionenv"
)

func TestProxyOverrideApprovalRetainsExistingPolicyAndRejectsBroadGrants(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("requires an enforcing sandbox")
	}
	withExecSandboxPolicy(t, true, true, true)
	previous := executionenv.DefaultRegistry()
	registry := executionenv.NewRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"HTTPS_PROXY=http://proxy.example.test:8080"}, "test", "person", nil)
	for _, outcome := range []string{"approve", "escalate", "full-auto"} {
		t.Run(outcome, func(t *testing.T) {
			judge := &fakeJudge{reply: `{"outcome":"` + outcome + `","risk_level":"low","user_authorization":"high","rationale":"The person explicitly requested direct access for this observation."}`}
			mode := ApprovalSmart
			if outcome == "full-auto" {
				mode = ApprovalFullAuto
			}
			runGrants := newRunApprovalGrantSet()
			scope := ExecutionScope{TenantID: "default", PersonID: outcome, RunID: outcome, ApprovalMode: mode, Judge: judge, EnvironmentSnapshotID: snapshot.ID, runGrants: runGrants}
			asks := 0
			scope.Approval = func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
				asks++
				if req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly || req.GrantClass != "" || len(req.RuleCandidates) != 0 {
					t.Fatalf("route override offered broad authority: %+v", req)
				}
				return ToolApprovalDecision{Outcome: ApprovalOutcomeTimedOut}, nil
			}
			cleanup := SetExecutionScope(outcome, scope)
			defer cleanup()
			args := map[string]interface{}{"_tenant_id": outcome, "_tool_name": "terminal", "command": "env -u HTTPS_PROXY gh pr view 19 --json state", "_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": true}
			// An old command-class grant cannot authorize removal of the route.
			runGrants.add(approvalPatternKeyForScope("terminal", args, configuredProxyOverrideReason("terminal", args), scope, true))
			runGrants.add(approvalPatternKeyForScope("terminal", args, "", scope, true))
			ran := false
			execute := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran = true; return "observed", nil })
			_, err := execute(args)
			if outcome == "escalate" {
				if err == nil || ran || asks != 1 || judge.calls != 1 {
					t.Fatalf("unanswered review dispatched: ran=%v asks=%d judge=%d err=%v", ran, asks, judge.calls, err)
				}
			} else if err != nil || !ran || asks != 0 || judge.calls != map[bool]int{true: 0, false: 1}[outcome == "full-auto"] {
				t.Fatalf("existing route authorization changed: ran=%v asks=%d judge=%d err=%v", ran, asks, judge.calls, err)
			}
		})
	}
}

func TestToolChildKeepsReachableProxyAndHonorsExplicitRemoval(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is unavailable")
	}
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "proxy-only.invalid" {
			t.Errorf("unexpected proxy target: %s", r.URL.Host)
		}
		requests.Add(1)
		fmt.Fprint(w, "PROXY_OK")
	}))
	defer proxy.Close()
	previous := executionenv.DefaultRegistry()
	registry := executionenv.NewRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"PATH=/usr/bin:/bin:/opt/homebrew/bin", "http_proxy=" + proxy.URL}, "test", "person", nil)
	root := t.TempDir()
	cleanup := SetExecutionScope("proxy-child", ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}, EnvironmentSnapshotID: snapshot.ID})
	defer cleanup()
	tool := NewExecuteCommandTool()
	args := map[string]interface{}{"_tenant_id": "proxy-child", "cwd": root, "sandbox": "host", "command": "curl --silent --show-error --max-time 2 http://proxy-only.invalid/fixture"}
	result, err := tool.ExecuteResult(args)
	if err != nil || result.Output != "PROXY_OK" || requests.Load() != 1 || result.Process.ProxyMode != proxyModeInherited {
		t.Fatalf("reachable proxy was lost: result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
	args["command"] = "unset http_proxy; curl --silent --show-error --max-time 2 http://proxy-only.invalid/fixture"
	result, err = tool.ExecuteResult(args)
	if err == nil || requests.Load() != 1 || len(result.Process.RequestedProxyRemovals) != 1 || result.Process.RequestedProxyRemovals[0] != "http_proxy" {
		t.Fatalf("explicit removal was rewritten or misreported: result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
	// A path-qualified wrapper performs the same removal and reports it.
	args["command"] = "/usr/bin/env -u http_proxy curl --silent --show-error --max-time 2 http://proxy-only.invalid/fixture"
	result, err = tool.ExecuteResult(args)
	if err == nil || requests.Load() != 1 || !reflect.DeepEqual(result.Process.RequestedProxyRemovals, []string{"http_proxy"}) {
		t.Fatalf("path-qualified removal was misreported: result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
	// This is per-child: explicit removal cannot mutate the frozen environment
	// used by the next command, or the operator's original proxy settings.
	args["command"] = "curl --silent --show-error --max-time 2 http://proxy-only.invalid/again"
	if result, err = tool.ExecuteResult(args); err != nil || result.Output != "PROXY_OK" || requests.Load() != 2 {
		t.Fatalf("removal escaped its child: result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
}

func TestProxyRemovalPreservesObservationProof(t *testing.T) {
	for _, command := range []string{
		"unset HTTPS_PROXY HTTP_PROXY ALL_PROXY https_proxy http_proxy all_proxy; gh pr view 2436 --repo owner/repo --json state 2>&1 | head -120",
		"unset -v https_proxy; aws sts get-caller-identity",
		"env -u HTTPS_PROXY -u HTTP_PROXY gh pr view 19 --json state",
		"env --unset=https_proxy --unset all_proxy -- gcloud builds describe build-2",
		"env -uHTTPS_PROXY command gh api repos/other/project/pulls/7",
	} {
		t.Run(command, func(t *testing.T) {
			if !provenReadOnlyWithCredentials(t, command) {
				t.Fatalf("literal proxy removal must not turn an observation into an external mutation: %s", command)
			}
		})
	}
	for _, command := range []string{
		"unset PATH; gh pr view 19",
		"unset BASH_ENV HTTPS_PROXY; gh pr view 19",
		"unset -f gh; gh pr view 19",
		"unset \"$NAME\"; gh pr view 19",
		"env -i gh pr view 19",
		"env -u PATH gh pr view 19",
		"env -u HTTPS_PROXY PATH=/tmp gh pr view 19",
		"env -u HTTPS_PROXY ./gh pr view 19",
		"unset HTTPS_PROXY; gh api -X DELETE repos/owner/repo",
		"env -u HTTPS_PROXY gh pr merge 19",
	} {
		if provenReadOnlyWithCredentials(t, command) {
			t.Errorf("unproven command must remain gated: %s", command)
		}
	}
}

// A case-insensitive filesystem runs `Env`, `BASH` and `/usr/bin/ENV` as the
// ordinary programs, and the observation prover already reads them that way.
// Detection must normalize the same way or the removal escapes its review.
func TestProxyRemovalDetectionNormalizesProgramsLikeTheProver(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"Env -u HTTPS_PROXY gh pr view 19 --json state", []string{"HTTPS_PROXY"}},
		{"BASH -c 'unset HTTPS_PROXY; gh pr view 19 --json state'", []string{"HTTPS_PROXY"}},
		{"/usr/bin/env -u HTTPS_PROXY gh pr view 19 --json state", []string{"HTTPS_PROXY"}},
		{"/usr/bin/ENV --unset=https_proxy gcloud builds describe build-2", []string{"https_proxy"}},
		{"/bin/Zsh -c 'unset -v all_proxy; aws sts get-caller-identity'", []string{"all_proxy"}},
		{"COMMAND Env -u HTTP_PROXY gh api repos/owner/repo/pulls/7", []string{"HTTP_PROXY"}},
		{"TIMEOUT 30 ENV -u HTTPS_PROXY gh pr view 19", []string{"HTTPS_PROXY"}},
		{"UNSET HTTPS_PROXY; gh pr view 19", []string{"HTTPS_PROXY"}},
		// Only the route variables are removals; other names stay out.
		{"Env -u PATH gh pr view 19", nil},
		{"BASH -c 'gh pr view 19'", nil},
	} {
		if got := proxyRemovalVariables(tc.command); !reflect.DeepEqual(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
			t.Errorf("%q removals=%v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestConfiguredProxyRemovalVariantsAreReviewedAndReported(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("requires an enforcing sandbox")
	}
	withExecSandboxPolicy(t, true, true, true)
	previous := executionenv.DefaultRegistry()
	registry := executionenv.NewRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy.example.test:8080"}, "test", "person", nil)
	for _, command := range []string{
		"Env -u HTTPS_PROXY gh pr view 19 --json state",
		"BASH -c 'unset HTTPS_PROXY; gh pr view 19 --json state'",
		"/usr/bin/ENV -u HTTPS_PROXY gh pr view 19 --json state",
	} {
		t.Run(command, func(t *testing.T) {
			judge := &fakeJudge{reply: "DENY"}
			cleanup := SetExecutionScope(command, ExecutionScope{
				TenantID: "default", PersonID: command, RunID: command,
				EnvironmentSnapshotID: snapshot.ID, ApprovalMode: ApprovalSmart, Judge: judge,
			})
			defer cleanup()
			ran := false
			execute := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran = true; return "observed", nil })
			_, err := execute(map[string]interface{}{
				"_tenant_id": command, "_tool_name": "terminal", "command": command,
				"_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": true, credentialReadArgKey: true,
			})
			if err == nil || ran || judge.calls != 1 || !strings.Contains(judge.lastArg, "configured proxy variables (HTTPS_PROXY)") {
				t.Fatalf("route override escaped review: ran=%v calls=%d err=%v prompt=%s", ran, judge.calls, err, judge.lastArg)
			}
		})
	}
}

// The route change is one more review fact. It must not replace what made the
// operation dangerous in either the judge prompt or the person's request.
func TestProxyReviewAppendsToTheDangerousReason(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("requires an enforcing sandbox")
	}
	withExecSandboxPolicy(t, true, true, true)
	previous := executionenv.DefaultRegistry()
	registry := executionenv.NewRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy.example.test:8080"}, "test", "person", nil)
	const proxyFact = "removes configured proxy variables (HTTPS_PROXY)"
	for _, tc := range []struct {
		name, command string
		parallel      bool
		deny          bool
		want, absent  []string
	}{
		{name: "egress", command: "unset HTTPS_PROXY; curl -X DELETE https://api.example.test/items/1", want: []string{"network egress command: curl", proxyFact}},
		{name: "destructive", command: "unset HTTPS_PROXY; rm -rf build; gh pr view 19 --json state", want: []string{"invokes dangerous command: rm", proxyFact}},
		{name: "wrapper variant", command: "env -u HTTPS_PROXY wget -q -O- https://api.example.test/export", want: []string{"network egress command: wget", proxyFact}},
		// A parallel Run is reviewed on the same facts as a single Run (D7).
		{name: "parallel run", command: "unset HTTPS_PROXY; curl -X DELETE https://api.example.test/items/1", parallel: true, want: []string{"network egress command: curl", proxyFact}, absent: []string{"parallel"}},
		{name: "explicit deny", command: "unset HTTPS_PROXY; rm -rf build; gh pr view 19 --json state", deny: true, want: []string{"explicit deny", "invokes dangerous command: rm", proxyFact}},
		// Facts that do not apply are not claimed.
		{name: "no route change", command: "curl -X DELETE https://api.example.test/items/1", want: []string{"network egress command: curl"}, absent: []string{"configured proxy"}},
		{name: "parallel only", command: "gh pr merge 19", parallel: true, want: []string{"requires approval in smart mode"}, absent: []string{"configured proxy", "parallel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			judge := &fakeJudge{reply: `{"outcome":"escalate","risk_level":"high","user_authorization":"unknown","rationale":"The deletion target is not authorized."}`}
			var asked []string
			scope := ExecutionScope{
				TenantID: "default", PersonID: tc.name, RunID: tc.name, ParallelWork: tc.parallel,
				EnvironmentSnapshotID: snapshot.ID, ApprovalMode: ApprovalSmart, Judge: judge,
				Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					asked = append(asked, req.Reason)
					return ToolApprovalDecision{Outcome: ApprovalOutcomeTimedOut}, nil
				},
			}
			if tc.deny {
				scope.IntentSnapshot = func() RunIntentSnapshot {
					return RunIntentSnapshot{ModelAuthorization: true, ExplicitDeny: []string{"do not delete anything"}}
				}
			}
			cleanup := SetExecutionScope(tc.name, scope)
			defer cleanup()
			execute := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ran", nil })
			if _, err := execute(map[string]interface{}{
				"_tenant_id": tc.name, "_tool_name": "terminal", "command": tc.command,
				"_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": true,
			}); err == nil {
				t.Fatal("an unanswered review dispatched")
			}
			if len(asked) != 1 {
				t.Fatalf("person was asked %d times", len(asked))
			}
			surfaces := map[string]string{"request": asked[0]}
			if judge.calls > 0 {
				surfaces["judge"] = judge.lastArg
			}
			for surface, text := range surfaces {
				for _, want := range tc.want {
					if !strings.Contains(text, want) {
						t.Errorf("%s lost %q: %s", surface, want, text)
					}
				}
				for _, absent := range tc.absent {
					if strings.Contains(text, absent) {
						t.Errorf("%s claimed %q: %s", surface, absent, text)
					}
				}
			}
		})
	}
}

func TestConfiguredProxyRemovalIsReviewedWithoutMisclassifyingTheRead(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("requires an enforcing sandbox")
	}
	withExecSandboxPolicy(t, true, true, true)
	previous := executionenv.DefaultRegistry()
	registry := executionenv.NewRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	for _, tc := range []struct {
		name, command string
		proxy         bool
		wantReview    bool
	}{
		{"inherit configured route", "gh pr view 19 --json state", true, false},
		{"remove configured route", "unset HTTPS_PROXY; gh pr view 19 --json state", true, true},
		{"wrapper removes configured route", "env -u HTTPS_PROXY gh pr view 19 --json state", true, true},
		{"nested wrappers remove configured route", "command env -u HTTP_PROXY env -u HTTPS_PROXY gh pr view 19 --json state", true, true},
		{"literal shell removes configured route", "sh -c 'unset HTTPS_PROXY; gh pr view 19 --json state'", true, true},
		{"no configured route to remove", "unset HTTPS_PROXY; gh pr view 19 --json state", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"PATH=/usr/bin"}
			if tc.proxy {
				env = append(env, "HTTPS_PROXY=http://proxy.example.test:8080")
			}
			snapshot := registry.Install(env, "test", "person", nil)
			judge := &fakeJudge{reply: "DENY"}
			cleanup := SetExecutionScope(tc.name, ExecutionScope{
				TenantID: "default", PersonID: tc.name, RunID: tc.name,
				EnvironmentSnapshotID: snapshot.ID, ApprovalMode: ApprovalSmart, Judge: judge,
			})
			defer cleanup()
			ran := false
			execute := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran = true; return "observed", nil })
			_, err := execute(map[string]interface{}{
				"_tenant_id": tc.name, "_tool_name": "terminal", "command": tc.command,
				"_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": true, credentialReadArgKey: true,
			})
			if tc.wantReview {
				if err == nil || ran || judge.calls != 1 || !strings.Contains(judge.lastArg, "configured proxy") {
					t.Fatalf("route override escaped review: ran=%v calls=%d err=%v prompt=%s", ran, judge.calls, err, judge.lastArg)
				}
				if strings.Contains(judge.lastArg, "proxy.example.test") {
					t.Fatal("review exposed a proxy endpoint")
				}
			} else if err != nil || !ran || judge.calls != 0 {
				t.Fatalf("ordinary observation gained review friction: ran=%v calls=%d err=%v", ran, judge.calls, err)
			}
		})
	}
}
