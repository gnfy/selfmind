package httpapi

import (
	"context"
	"runtime"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// Regression for the 2026-07-17 incident: gcloud/aws CLI output carries a
// trailing newline, and anchored patterns like ^SUCCESS$ never matched raw
// output, so finished builds were reported as watch timeouts.
func TestMatchesExternalWatchPatternNormalizesOutput(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		output  string
		want    bool
	}{
		{"anchored with trailing newline", "^SUCCESS$", "SUCCESS\n", true},
		{"anchored with crlf", "^SUCCESS$", "SUCCESS\r\n", true},
		{"anchored with surrounding spaces", "^SUCCESS$", "  SUCCESS  \n", true},
		{"anchored multiline match on line", "^SUCCEEDED$", "status check\nSUCCEEDED\n", true},
		{"unanchored still works", "SUCCEEDED", "SUCCEEDED\n", true},
		{"failure alternation with newline", "^(FAILURE|INTERNAL_ERROR|TIMEOUT|CANCELLED|EXPIRED)$", "FAILURE\n", true},
		{"non-terminal state does not match", "^SUCCESS$", "WORKING\n", false},
		{"substring must not satisfy anchored pattern", "^SUCCESS$", "SUCCESSFUL\n", false},
		{"empty output", "^SUCCESS$", "", false},
		{"empty pattern never matches", "", "SUCCESS\n", false},
		{"blank pattern never matches", "   ", "SUCCESS\n", false},
		{"invalid regexp never matches", "([", "SUCCESS\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesExternalWatchPattern(tc.pattern, tc.output); got != tc.want {
				t.Fatalf("matchesExternalWatchPattern(%q, %q) = %v, want %v", tc.pattern, tc.output, got, tc.want)
			}
		})
	}
}

func TestExternalWatchIDFromFinalizationKey(t *testing.T) {
	if got := externalWatchIDFromFinalizationKey("external-watch:watch_123:r2:finalization"); got != "watch_123" {
		t.Fatalf("watch id = %q", got)
	}
	for _, key := range []string{
		"",
		"external-watch:watch_123:finalization",
		"other:watch_123:r2:finalization",
	} {
		if got := externalWatchIDFromFinalizationKey(key); got != "" {
			t.Fatalf("invalid key %q produced watch id %q", key, got)
		}
	}
}

func TestExternalWatchCompletionNoticeUsesStableID(t *testing.T) {
	cases := map[string]string{
		control.ExternalWatchSucceeded: "Watcher watch_123 | status: succeeded | task: waiting_finalization",
		control.ExternalWatchFailed:    "Watcher watch_123 | status: failed | task: waiting_finalization",
		control.ExternalWatchTimedOut:  "Watcher watch_123 | status: timed_out | task: waiting_finalization",
		control.ExternalWatchCancelled: "Watcher watch_123 | status: cancelled | task: waiting_finalization",
	}
	for status, want := range cases {
		if got := externalWatchCompletionNotice("watch_123", status, "waiting_finalization"); got != want {
			t.Fatalf("status %q notice = %q, want %q", status, got, want)
		}
	}
}

func TestRunExternalWatchCommandUsesBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	server := &Server{}
	result, err := server.runExternalWatchCommand(context.Background(), control.ExternalWatch{
		CWD: t.TempDir(), Command: "set -euo pipefail; printf SUCCESS",
	})
	if err != nil {
		t.Fatalf("bash watch command failed: %v (%s)", err, result.Output)
	}
	if result.Output != "SUCCESS" {
		t.Fatalf("output = %q, want SUCCESS", result.Output)
	}
	if result.FailureClass != "" || result.ExitCode != 0 {
		t.Fatalf("clean check reported class %q exit %d", result.FailureClass, result.ExitCode)
	}
}

func TestExternalWatchCommandDoesNotInheritControlPlaneSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	t.Setenv("SELF_GATEWAY_TOKEN", "must-not-leak")
	tools.SetExecSandbox(false, false, false)
	t.Cleanup(func() { tools.SetExecSandbox(false, false, false) })

	server := &Server{}
	result, err := server.runExternalWatchCommand(context.Background(), control.ExternalWatch{
		CWD:     t.TempDir(),
		Command: `printf '%s' "${SELF_GATEWAY_TOKEN:-}"`,
	})
	if err != nil {
		t.Fatalf("watch command failed: %v (%s)", err, result.Output)
	}
	if result.Output != "" {
		t.Fatalf("watcher inherited control-plane secret: %q", result.Output)
	}
}

func TestClassifyExternalWatchOutput(t *testing.T) {
	watch := control.ExternalWatch{
		SuccessPattern: "^SUCCESS$",
		FailurePattern: "^(FAILURE|CANCELLED)$",
	}
	if got := classifyExternalWatchOutput(watch, "SUCCESS\n", 0); got != control.ExternalWatchSucceeded {
		t.Fatalf("success output classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "CANCELLED\n", 0); got != control.ExternalWatchFailed {
		t.Fatalf("failure output classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "QUEUED\n", 0); got != "" {
		t.Fatalf("non-terminal output classified as %q", got)
	}
	noFailure := control.ExternalWatch{SuccessPattern: "^SUCCESS$"}
	if got := classifyExternalWatchOutput(noFailure, "FAILURE\n", 0); got != "" {
		t.Fatalf("output without failure pattern classified as %q", got)
	}
	// A check that itself exited non-zero must not be able to declare success:
	// a "SUCCESS" printed by a broken check is contradictory evidence.
	if got := classifyExternalWatchOutput(watch, "SUCCESS\n", 1); got != "" {
		t.Fatalf("success from a failed check classified as %q", got)
	}
	// A terminal FAILURE reported with a non-zero exit stays valid: status CLIs
	// legitimately exit non-zero while reporting a real failure.
	if got := classifyExternalWatchOutput(watch, "FAILURE\n", 1); got != control.ExternalWatchFailed {
		t.Fatalf("failure with non-zero exit classified as %q", got)
	}
}

func TestClassifyExternalWatchOutputV2PrioritizesTerminalFailure(t *testing.T) {
	watch := control.ExternalWatch{
		SpecVersion:            2,
		TargetPattern:          "PENDING_APPROVAL",
		TerminalSuccessPattern: "SUCCEEDED",
		TerminalFailurePattern: "FAILED",
	}
	if got := classifyExternalWatchOutput(watch, "PENDING_APPROVAL\nFAILED\n", 0); got != control.ExternalWatchFailed {
		t.Fatalf("failure and target classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "PENDING_APPROVAL\n", 0); got != control.ExternalWatchSucceeded {
		t.Fatalf("target classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "SUCCEEDED\n", 0); got != control.ExternalWatchSucceeded {
		t.Fatalf("terminal success classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "PENDING_APPROVAL\n", 1); got != "" {
		t.Fatalf("non-clean target classified as %q", got)
	}
}
