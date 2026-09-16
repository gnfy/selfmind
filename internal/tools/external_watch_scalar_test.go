package tools

import (
	"errors"
	"strings"
	"testing"
)

func TestExternalWatchScalarRejectsNestedAndPartialSuccess(t *testing.T) {
	for _, output := range []string{`{"status":"WORKING","steps":[{"status":"SUCCESS"}]}`, "WORKING\nSUCCESS", "", strings.Repeat("x", 4097)} {
		if ValidateExternalWatchScalar(output) == nil || MatchExternalWatchScalar("SUCCESS", output) {
			t.Fatalf("ambiguous evidence accepted: %.100s", output)
		}
	}
	if MatchExternalWatchScalar("SUCCESS", "NOT_SUCCESS") {
		t.Fatal("partial match accepted")
	}
	if !MatchExternalWatchScalar("SUCCESS|DONE", " DONE\n") {
		t.Fatal("selected scalar rejected")
	}
}

func TestNewWatchPreflightCapturesTerminalGroupEvidence(t *testing.T) {
	for _, state := range []string{"SUCCESS", "FAILED", "PENDING"} {
		observed := externalWatchPreflightObservation{}
		_, err := preflightExternalWatchPatterns(preflightArgs("printf "+state), "printf "+state, t.TempDir(), externalWatchPreflightPatterns{Scalar: true, Success: "SUCCESS", Failure: "FAILED", Observation: &observed}, 5)
		if state == "FAILED" && err == nil {
			t.Fatal("standalone failure lost")
		}
		if state != "FAILED" && err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"SUCCESS": "succeeded", "FAILED": "failed", "PENDING": ""}[state]
		if observed.Status != want || observed.Output != state {
			t.Fatalf("observation=%+v", observed)
		}
	}
	observed := externalWatchPreflightObservation{}
	command := `printf '%s' '{"status":"WORKING","steps":[{"status":"SUCCESS"}]}'`
	_, err := preflightExternalWatchPatterns(preflightArgs(command), command, t.TempDir(), externalWatchPreflightPatterns{Scalar: true, Success: "SUCCESS", Observation: &observed}, 5)
	if err == nil || observed.Status != "" {
		t.Fatalf("nested success accepted: %+v %v", observed, err)
	}
}

func TestTerminalSyntaxRejectedBeforeApprovalOrPartialExecution(t *testing.T) {
	reached := false
	execute := NewToolGuardrails().Middleware(func(map[string]interface{}) (string, error) { reached = true; return "", nil })
	_, err := execute(map[string]interface{}{"_tool_name": "terminal", "command": "printf first; printf %s (READY)"})
	var detail interface {
		ToolErrorCode() string
		ToolEffectState() string
	}
	if reached || !errors.As(err, &detail) || detail.ToolErrorCode() != "command_syntax" || detail.ToolEffectState() != "not_dispatched" {
		t.Fatalf("reached=%v err=%v", reached, err)
	}
	if _, err := execute(map[string]interface{}{"_tool_name": "terminal", "command": "printf %s '(READY)'"}); err != nil || !reached {
		t.Fatalf("valid correction: %v", err)
	}
}

func TestIntermediateGroupObservationDoesNotClaimOperationSuccess(t *testing.T) {
	observed := externalWatchPreflightObservation{}
	command := "printf APPROVAL"
	reply, err := preflightExternalWatchPatterns(preflightArgs(command), command, t.TempDir(), externalWatchPreflightPatterns{Scalar: true, Target: "APPROVAL", TerminalSuccess: "DONE", TerminalFailure: "FAILED", Observation: &observed}, 5)
	if err != nil || reply == "" || observed.Status != "succeeded" || observed.OperationStatus != "running" {
		t.Fatalf("intermediate condition promoted to operation success: %+v %v", observed, err)
	}
}

func TestPreflightRecordsActualApprovedHostNetworkBoundary(t *testing.T) {
	policy := CurrentExecSandboxPolicy()
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(policy.Enabled, policy.Required, policy.AllowNetwork) })
	observed := externalWatchPreflightObservation{}
	command := "printf PENDING"
	_, err := preflightExternalWatchPatterns(preflightArgs(command), command, t.TempDir(), externalWatchPreflightPatterns{Scalar: true, Success: "DONE", Observation: &observed}, 5)
	if err != nil || !observed.HostNetworkShared {
		t.Fatalf("host boundary lost: %+v %v", observed, err)
	}
	failed := externalWatchPreflightObservation{}
	_, err = preflightExternalWatchPatterns(preflightArgs("exit 1"), "exit 1", t.TempDir(), externalWatchPreflightPatterns{Scalar: true, Success: "DONE", Observation: &failed}, 5)
	if err == nil || failed.HostNetworkShared {
		t.Fatalf("failed preflight recorded usable authority: %+v %v", failed, err)
	}
}
