package tools

import (
	"strings"
	"testing"
)

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
	if called {
		t.Fatal("active-turn polling reached the executor")
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
