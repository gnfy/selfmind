package tools

import (
	"runtime"
	"strings"
	"testing"
)

func preflightArgs(command string) map[string]interface{} {
	return map[string]interface{}{"_tool_name": "watch_external", "command": command}
}

// A check that already reports success has nothing to wait for. Registering a
// watch would park the task until the next tick for no reason.
func TestPreflightReportsAlreadySucceeded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "printf 'SUCCEEDED\\n'"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(verdict, "not registered") || !strings.Contains(verdict, "nothing to wait for") {
		t.Fatalf("verdict = %q", verdict)
	}
}

// The ambiguous case, and the one that caused real damage: the very first check
// reports the failure pattern. It may be reporting its own inability to query
// (the live GCP check printed BUILD_FAILED while both builds had SUCCEEDED), so
// the agent must verify now rather than let the daemon record a failure.
func TestPreflightRefusesFirstCheckFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "printf 'BUILD_FAILED\\n'"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "ALL_SUCCESS", "BUILD_FAILED", 10)
	if verdict != "" {
		t.Fatalf("a first-check failure must not be a benign verdict: %q", verdict)
	}
	if err == nil {
		t.Fatal("a first-check failure must come back as an error the agent can act on")
	}
	if !strings.Contains(err.Error(), "Verify the external state directly") {
		t.Fatalf("error = %v", err)
	}
}

// An undefined executable is a defect in the check itself. Repeating it in the
// background cannot reveal anything.
func TestPreflightRefusesDefectiveCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "selfmind-no-such-cli status"
	_, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "", 10)
	if err == nil {
		t.Fatal("a check whose executable does not exist must not be registered")
	}
	if !strings.Contains(err.Error(), "watch not registered") || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("error = %v", err)
	}
}

// A pending check is the normal case: register the watch.
func TestPreflightAllowsPendingCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "printf 'WORKING\\n'"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 10)
	if err != nil || verdict != "" {
		t.Fatalf("a pending check must register: verdict=%q err=%v", verdict, err)
	}
}

// A status CLI that exits non-zero while its operation converges must still be
// watchable: only diagnosable environment/definition failures block.
func TestPreflightAllowsOrdinaryNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "printf 'STILL RUNNING\\n'; exit 1"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 10)
	if err != nil || verdict != "" {
		t.Fatalf("an ordinary non-zero status check must register: verdict=%q err=%v", verdict, err)
	}
}

func TestPreflightBlockingClasses(t *testing.T) {
	for _, class := range []string{
		"credential_state_readonly", "sandbox_fs_denied", "permission",
		"credential_missing", "credential_expired", "auth", "environment",
		"syntax", "not_found",
	} {
		if !preflightBlockingClass(class) {
			t.Fatalf("%s must block registration", class)
		}
	}
	for _, class := range []string{"timeout", "network", "unknown", ""} {
		if preflightBlockingClass(class) {
			t.Fatalf("%s must not block registration", class)
		}
	}
}
