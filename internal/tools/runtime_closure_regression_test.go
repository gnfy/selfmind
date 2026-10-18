package tools

import (
	"errors"
	"selfmind/internal/kernel"
	"testing"
)

func TestRuntimeClosureUnrelatedObservationMustNotReleaseEffect(t *testing.T) {
	for _, observation := range []string{"cat unrelated.txt", "ps -ef"} {
		t.Run(observation, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			f.registry.Register(newLifecycleExecTool("terminal", func(command string) (kernel.ToolDispatchResult, error) {
				if command == "npm publish" {
					return exitedResult(1), errors.New("remote operation returned without success proof")
				}
				return exitedResult(0), nil
			}))
			run := f.runs[0]
			if _, err := f.dispatch(run, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
				t.Fatal("expected uncertain publish")
			}
			before := f.unresolved()
			if len(before) != 1 {
				t.Fatalf("need one uncertain effect, got %d", len(before))
			}
			result, err := f.dispatch(run, "terminal", "unrelated-check", map[string]interface{}{"command": observation})
			if err != nil {
				t.Fatal(err)
			}
			after := f.unresolved()
			if len(after) != 1 {
				t.Fatalf("UNRELATED observation released publish effect: remaining=%d result=%s", len(after), result.Output)
			}
		})
	}
}

func TestRuntimeClosureRunGrantCannotBypassNewSemanticRestriction(t *testing.T) {
	// Shared-network execution is deliberately uncontained. Offline containment
	// is a separate allowed bypass and would not exercise cached-grant ordering.
	withExecSandboxPolicy(t, true, true, true)
	judge := &fakeJudge{reply: `{"risk_level":"low","user_authorization":"low","outcome":"deny","rationale":"The person now requires the script to remain unchanged."}`}
	scope := ExecutionScope{TenantID: "closure-grant", PersonID: "closure-grant", RunID: "closure-grant", ApprovalMode: ApprovalSmart, Judge: judge, runGrants: newRunApprovalGrantSet(), IntentSnapshot: func() RunIntentSnapshot {
		return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Leave the script unchanged."}
	}}
	args := map[string]interface{}{"command": "chmod +x script.sh"}
	_, reason := dangerousToolCall("", "terminal", args)
	key := approvalPatternKeyForScope("terminal", args, reason, scope, true)
	if key == "" {
		t.Fatal("missing grant fixture")
	}
	scope.runGrants.add(key)
	ran, err := runSmart(t, scope, "closure-grant", "chmod +x script.sh")
	if ran || judge.calls != 1 || err == nil {
		t.Fatalf("old grant bypassed new restriction: ran=%v judge_calls=%d err=%v", ran, judge.calls, err)
	}
}

func TestRuntimeClosureSkillClassMustNotCollapseDifferentScripts(t *testing.T) {
	for _, interpreter := range []string{"bash", "python3", "node"} {
		for _, script := range []string{"/tmp/one/skills/release/scripts/trigger.sh --project staging", "/tmp/two/skills/release/scripts/trigger.sh --project prod"} {
			if label, ok := prefixOf(t, interpreter+" "+script); ok {
				t.Fatalf("script paths cannot mint reusable authority: %q", label)
			}
		}
	}
}

func TestRuntimeClosureHostnameSetterIsNotReadOnly(t *testing.T) {
	for _, command := range []string{"hostname changed-by-probe", "hostname -F hostfile", "hostname -b name"} {
		if observationOnlyExec("terminal", map[string]interface{}{"command": command}) {
			t.Fatalf("hostname mutation classified read-only: %s", command)
		}
	}
	if !observationOnlyExec("terminal", map[string]interface{}{"command": "hostname"}) {
		t.Fatal("hostname query lost read-only proof")
	}
}
