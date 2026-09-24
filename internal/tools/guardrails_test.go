package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type changingPlanRevision struct{ revision string }

func (*changingPlanRevision) Project(context.Context, PlanState) (PlanProjectionResult, error) {
	return PlanProjectionResult{}, nil
}
func (*changingPlanRevision) ValidateCompletion(context.Context) error { return nil }
func (p *changingPlanRevision) GuardrailRevision(context.Context, []string) (string, error) {
	return p.revision, nil
}

func TestToolGuardrailsRetryIdenticalPlanOnlyAfterEvidenceChanges(t *testing.T) {
	projection := &changingPlanRevision{revision: "plan-1:proof-0"}
	ctx := WithRunPlanProjection(context.Background(), projection)
	called := 0
	exec := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) {
		called++
		if projection.revision == "plan-1:proof-1" {
			return "completed", nil
		}
		return "", errors.New("verification required")
	})
	args := map[string]interface{}{"_tool_name": "update_plan", "_context": ctx, "_tenant_id": "tenant-a", "plan": []interface{}{"same complete snapshot"}}
	for i := 0; i < 2; i++ {
		if _, err := exec(args); err == nil {
			t.Fatalf("attempt %d unexpectedly passed", i+1)
		}
	}
	if _, err := exec(args); err == nil || !strings.Contains(err.Error(), "repeated failure") || called != 2 {
		t.Fatalf("unchanged evidence was dispatched: calls=%d err=%v", called, err)
	}
	projection.revision = "plan-1:proof-1"
	if got, err := exec(args); err != nil || got != "completed" || called != 3 {
		t.Fatalf("new evidence did not release the exact plan: calls=%d result=%q err=%v", called, got, err)
	}
}

func TestToolGuardrailsBlockActiveTurnRemotePolling(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "RUNNING", nil
	})

	_, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    "while true; do gcloud builds describe build-1; sleep 30; done",
	})
	if err == nil || !strings.Contains(err.Error(), "watch_external") {
		t.Fatalf("polling rejection = %v", err)
	}
	var facts interface {
		ToolErrorCategory() string
		ToolErrorCode() string
		ToolEffectState() string
	}
	if !errors.As(err, &facts) || facts.ToolErrorCategory() != "policy_redirect" || facts.ToolErrorCode() != "active_turn_polling" || facts.ToolEffectState() != "not_dispatched" {
		t.Fatalf("polling should be a typed policy redirect, got %v", err)
	}
	if called {
		t.Fatal("active-turn polling reached the executor")
	}
}

func TestToolGuardrailsBlockFiniteWaitForSameExternalTarget(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) {
		called = true
		return "UNKNOWN", nil
	})
	_, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `for i in 1 2 3 4 5 6; do gh pr view 123 --json mergeable; sleep 10; done`,
	})
	if err == nil || !strings.Contains(err.Error(), "watch_external") || called {
		t.Fatalf("bounded repeated PR wait was dispatched: err=%v called=%v", err, called)
	}
}

func TestToolGuardrailsSeeExternalObservationThroughProcessWrappers(t *testing.T) {
	for _, command := range []string{
		`for i in 1 2 3; do env -u HTTPS_PROXY gh pr view 123 --json mergeable; done`,
		`for i in 1 2 3; do timeout 5 /opt/bin/gh pr view 123 --json mergeable; done`,
	} {
		called := false
		exec := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) {
			called = true
			return "UNKNOWN", nil
		})
		_, err := exec(map[string]interface{}{"_tool_name": "terminal", "command": command})
		if err == nil || !strings.Contains(err.Error(), "watch_external") || called {
			t.Fatalf("wrapped poll reached execution: command=%q err=%v called=%v", command, err, called)
		}
	}
}

func TestToolGuardrailsAllowFiniteDistinctExternalObservations(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) {
		called = true
		return "results", nil
	})
	_, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `for pr in 123 124 125; do gh pr view "$pr" --json mergeable; sleep 1; done`,
	})
	if err != nil || !called {
		t.Fatalf("distinct PR observations were refused: err=%v called=%v", err, called)
	}
}

func TestToolGuardrailsDoNotTreatRepeatedMutationAsReadOnlyStatus(t *testing.T) {
	if isRemoteObservationCommand(`for i in 1 2 3; do gh pr merge 123; sleep 5; done`) {
		t.Fatal("mutation was classified as an external observation")
	}
}

// Provider-native waits and convention-named cloud reads are outside the
// approval catalog, yet a loop around them is still polling. The last command
// reaches another AWS service through the same naming convention.
func TestToolGuardrailsBlockPollingOutsideApprovalCatalog(t *testing.T) {
	commands := []string{
		`while true; do kubectl rollout status deploy/api -n prod --timeout=10s; sleep 10; done`,
		`until kubectl wait --for=condition=available deploy/api --timeout=5s; do sleep 5; done`,
		`for i in 1 2 3 4 5; do argocd app wait web --health --timeout 30; sleep 5; done`,
		`while :; do gh run watch 123 --exit-status; sleep 30; done`,
		`for i in $(seq 1 20); do aws cloudformation describe-stacks --stack-name web; sleep 15; done`,
		`while true; do az deployment group show -g rg -n web --query properties.provisioningState; sleep 20; done`,
		`while true; do aws ssm get-command-invocation --command-id c1 --instance-id i-1; sleep 5; done`,
	}
	for _, command := range commands {
		called := false
		exec := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) {
			called = true
			return "IN_PROGRESS", nil
		})
		_, err := exec(map[string]interface{}{"_tool_name": "terminal", "command": command})
		if err == nil || !strings.Contains(err.Error(), "watch_external") || called {
			t.Fatalf("polling reached execution: command=%q err=%v called=%v", command, err, called)
		}
	}
	// Recognition for this guard only restricts; the observations themselves
	// still require approval.
	for _, command := range []string{
		`kubectl rollout status deploy/api -n prod --timeout=10s`,
		`az deployment group show -g rg -n web`,
		`aws ssm get-command-invocation --command-id c1 --instance-id i-1`,
	} {
		if deterministicObservationExec("terminal", map[string]interface{}{"command": command}) {
			t.Fatalf("polling recognition widened automatic approval: %q", command)
		}
	}
}

// One native wait is the alternative the refusal recommends. Retrying an
// effect and reading distinct targets are not polling either.
func TestToolGuardrailsAllowNativeWaitAndEffectRetries(t *testing.T) {
	for _, command := range []string{
		`kubectl rollout status deploy/api -n prod --timeout=300s`,
		`aws cloudformation wait stack-update-complete --stack-name web`,
		`for i in 1 2 3; do kubectl rollout restart deploy/api && break; sleep 5; done`,
		`until aws cloudformation update-stack --stack-name web --use-previous-template; do sleep 10; done`,
		`until az deployment group create -g rg --template-file main.bicep; do sleep 10; done`,
		`for stack in web api; do aws cloudformation describe-stacks --stack-name "$stack"; done`,
	} {
		called := false
		exec := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) {
			called = true
			return "ok", nil
		})
		if _, err := exec(map[string]interface{}{"_tool_name": "terminal", "command": command}); err != nil || !called {
			t.Fatalf("non-polling command was refused: command=%q err=%v called=%v", command, err, called)
		}
	}
}

func TestToolGuardrailsAllowOneShotRemoteStatus(t *testing.T) {
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		return "RUNNING", nil
	})

	result, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    "gcloud builds describe build-1",
	})
	if err != nil || result != "RUNNING" {
		t.Fatalf("one-shot status result=%q err=%v", result, err)
	}
}

func TestToolGuardrailsAllowFiniteRemoteStatusBatch(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "RUNNING\nSUCCEEDED", nil
	})

	result, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `for build in build-1 build-2; do aws codebuild batch-get-builds --ids "$build"; done`,
	})
	if err != nil || result != "RUNNING\nSUCCEEDED" {
		t.Fatalf("finite status batch result=%q err=%v", result, err)
	}
	if !called {
		t.Fatal("finite status batch did not reach the executor")
	}
}

// One bounded remote read per item of a dynamic list is a fan-out, not
// polling. This exact shape was blocked live during a read-only release
// preflight and cost the run its correction budget.
func TestToolGuardrailsAllowDynamicListFanOut(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "SUCCEEDED", nil
	})
	result, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `ids=$(aws codebuild list-builds-for-project --project-name api --query ids --output text); for id in $ids; do aws codebuild batch-get-builds --ids "$id"; done`,
	})
	if err != nil || result != "SUCCEEDED" || !called {
		t.Fatalf("dynamic fan-out result=%q err=%v called=%v", result, err, called)
	}
}

// The same dynamic loop with a sleep in its body is an active wait and stays
// blocked; the refusal is typed as not dispatched so the recovery policy does
// not count it as a failed strategy attempt.
func TestToolGuardrailsBlockDynamicListPollingWithSleep(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "RUNNING", nil
	})
	_, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `for id in $ids; do aws codebuild batch-get-builds --ids "$id"; sleep 20; done`,
	})
	if err == nil || !strings.Contains(err.Error(), "watch_external") || called {
		t.Fatalf("sleeping dynamic loop must be blocked before execution: err=%v called=%v", err, called)
	}
	typed, ok := err.(interface{ ToolEffectState() string })
	if !ok || typed.ToolEffectState() != "not_dispatched" {
		t.Fatalf("guardrail refusal must be typed not_dispatched: %T %v", err, err)
	}
}

func TestToolGuardrailsBlockDetachedNestedShellPolling(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "started", nil
	})
	_, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `nohup bash -c 'for i in $(seq 1 90); do aws codebuild batch-get-builds --ids build-1; sleep 15; done' >/tmp/build.log 2>&1 &`,
	})
	if err == nil || !strings.Contains(err.Error(), "watch_external") || called {
		t.Fatalf("detached nested polling must be blocked before execution: err=%v called=%v", err, called)
	}
}

func TestToolGuardrailsBlockUnboundedCStyleRemotePolling(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "RUNNING", nil
	})

	_, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    `for ((;;)); do aws codebuild batch-get-builds --ids build-1; sleep 5; done`,
	})
	if err == nil || !strings.Contains(err.Error(), "provider-native wait") {
		t.Fatalf("unbounded polling rejection = %v", err)
	}
	if called {
		t.Fatal("unbounded polling reached the executor")
	}
}

func TestToolGuardrailsBlockRepeatedRemoteStatusWithoutProgress(t *testing.T) {
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		return "RUNNING", nil
	})
	args := map[string]interface{}{
		"_tool_name": "terminal",
		"command":    "gcloud builds describe build-1",
	}
	for i := 0; i < 3; i++ {
		if _, err := exec(args); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := exec(args); err == nil || !strings.Contains(err.Error(), "watch_external") {
		t.Fatalf("fourth repeated status check = %v", err)
	}
}

func TestToolGuardrailsDoNotTreatLocalShellLoopAsExternalWait(t *testing.T) {
	called := false
	exec := NewToolGuardrails().Middleware(func(args map[string]interface{}) (string, error) {
		called = true
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tool_name": "terminal",
		"command":    "for f in *.go; do gofmt -w \"$f\"; done",
	}); err != nil {
		t.Fatalf("local loop rejected: %v", err)
	}
	if !called {
		t.Fatal("local loop did not reach executor")
	}
}
