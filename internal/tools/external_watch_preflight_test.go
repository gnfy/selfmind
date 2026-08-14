package tools

import (
	"runtime"
	"strings"
	"testing"
	"time"
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

// A non-zero first check is not a valid durable contract. The agent can retry
// or repair it in the foreground; the daemon must not freeze and repeat it.
func TestPreflightRefusesOrdinaryNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "printf 'STILL RUNNING\\n'; exit 1"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 10)
	if err == nil || verdict != "" {
		t.Fatalf("a non-zero status check must be repaired before registration: verdict=%q err=%v", verdict, err)
	}
	if !strings.Contains(err.Error(), "exits 0") {
		t.Fatalf("error must state the clean-exit contract: %v", err)
	}
}

func TestPreflightRefusesSwallowedScriptFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "printf 'Traceback (most recent call last):\\nKeyError: status\\n'; exit 0"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 10)
	if err == nil || verdict != "" {
		t.Fatalf("a swallowed script failure must be rejected: verdict=%q err=%v", verdict, err)
	}
	if !strings.Contains(err.Error(), "check-definition") {
		t.Fatalf("error must identify the invalid check definition: %v", err)
	}
}

func TestPreflightRefusesEmptySuccessfulCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "true"
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 10)
	if err == nil || verdict != "" {
		t.Fatalf("an empty check cannot provide durable evidence: verdict=%q err=%v", verdict, err)
	}
}

func TestPreflightRefusesCheckThatAlreadyTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "sleep 2; printf 'WORKING\\n'"
	started := time.Now()
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 1)
	if verdict != "" || err == nil {
		t.Fatalf("over-budget check must be rejected: verdict=%q err=%v", verdict, err)
	}
	if !strings.Contains(err.Error(), "Split aggregate or multi-target checks") {
		t.Fatalf("error must tell the agent how to make the check bounded: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("preflight did not enforce its budget: %s", time.Since(started))
	}
}

func TestPreflightAllowsBoundedSlowObservation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(false, false, false) })

	command := "sleep 2; printf 'WORKING\\n'"
	started := time.Now()
	verdict, err := preflightExternalWatch(preflightArgs(command), command, t.TempDir(), "SUCCEEDED", "FAILED", 3)
	if err != nil || verdict != "" {
		t.Fatalf("bounded slow observation must register: verdict=%q err=%v", verdict, err)
	}
	if elapsed := time.Since(started); elapsed < 2*time.Second || elapsed > 4*time.Second {
		t.Fatalf("preflight duration = %s, want bounded slow execution", elapsed)
	}
}

func TestPreflightBlockingClasses(t *testing.T) {
	for _, class := range []string{
		"credential_state_readonly", "sandbox_fs_denied", "permission",
		"credential_missing", "credential_expired", "auth", "environment",
		"syntax", "not_found", "timeout", "check_definition",
	} {
		if !preflightBlockingClass(class) {
			t.Fatalf("%s must block registration", class)
		}
	}
	for _, class := range []string{"network", "unknown", ""} {
		if preflightBlockingClass(class) {
			t.Fatalf("%s must not block registration", class)
		}
	}
}
