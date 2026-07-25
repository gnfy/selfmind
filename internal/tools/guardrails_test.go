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
