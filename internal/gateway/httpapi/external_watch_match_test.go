package httpapi

import (
	"context"
	"runtime"
	"testing"

	"selfmind/internal/control"
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

func TestRunExternalWatchCommandUsesBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	output, err := runExternalWatchCommand(context.Background(), t.TempDir(), "set -euo pipefail; printf SUCCESS")
	if err != nil {
		t.Fatalf("bash watch command failed: %v (%s)", err, output)
	}
	if output != "SUCCESS" {
		t.Fatalf("output = %q, want SUCCESS", output)
	}
}

func TestClassifyExternalWatchCommandDefect(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		errText string
		want    bool
	}{
		{"illegal shell option", "sh: 1: set: Illegal option -o pipefail", "exit status 2", true},
		{"syntax error", "bash: syntax error near unexpected token `)'", "exit status 2", true},
		{"missing command", "bash: gcloudx: command not found", "exit status 127", true},
		{"ordinary nonzero is retryable", "BUILD STILL RUNNING", "exit status 1", false},
		{"transient network error is retryable", "connection reset by peer", "exit status 1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyExternalWatchCommandDefect(tc.output, tc.errText) != ""
			if got != tc.want {
				t.Fatalf("classifyExternalWatchCommandDefect(%q, %q) = %v, want %v", tc.output, tc.errText, got, tc.want)
			}
		})
	}
}

func TestClassifyExternalWatchOutput(t *testing.T) {
	watch := control.ExternalWatch{
		SuccessPattern: "^SUCCESS$",
		FailurePattern: "^(FAILURE|CANCELLED)$",
	}
	if got := classifyExternalWatchOutput(watch, "SUCCESS\n"); got != control.ExternalWatchSucceeded {
		t.Fatalf("success output classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "CANCELLED\n"); got != control.ExternalWatchFailed {
		t.Fatalf("failure output classified as %q", got)
	}
	if got := classifyExternalWatchOutput(watch, "QUEUED\n"); got != "" {
		t.Fatalf("non-terminal output classified as %q", got)
	}
	noFailure := control.ExternalWatch{SuccessPattern: "^SUCCESS$"}
	if got := classifyExternalWatchOutput(noFailure, "FAILURE\n"); got != "" {
		t.Fatalf("output without failure pattern classified as %q", got)
	}
}
