package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/control"
)

// Regression for the 2026-07-30 AWS watcher: the sandbox could not be built, the
// engine classified it as credential_state_readonly, and the watcher retried the
// identical failure 65 times until its two-hour deadline because it re-derived a
// diagnosis from text with its own marker list. An environment failure must park
// the watch on the FIRST occurrence.
func TestClassifyWatchCheckParksEnvironmentFailures(t *testing.T) {
	for _, class := range []string{
		"credential_state_readonly",
		"sandbox_fs_denied",
		"permission",
		"credential_missing",
		"credential_expired",
		"auth",
		"environment",
	} {
		verdict := classifyWatchCheck(class, 1, true)
		if verdict.Action != watchCheckPark {
			t.Fatalf("class %q action = %q, want park", class, verdict.Action)
		}
		if verdict.Reason != watchReasonBlockedEnvironment {
			t.Fatalf("class %q reason = %q, want %q", class, verdict.Reason, watchReasonBlockedEnvironment)
		}
		if verdict.Layer != "environment" {
			t.Fatalf("class %q layer = %q, want environment", class, verdict.Layer)
		}
	}
}

func TestClassifyWatchCheckParksDefectiveChecks(t *testing.T) {
	for _, class := range []string{"syntax", "not_found"} {
		verdict := classifyWatchCheck(class, 127, true)
		if verdict.Action != watchCheckPark || verdict.Reason != watchReasonInvalidCheck {
			t.Fatalf("class %q verdict = %+v, want an invalid_check park", class, verdict)
		}
	}
}

func TestClassifyWatchCheckRetriesTransientFailures(t *testing.T) {
	for _, class := range []string{"timeout", "network"} {
		if verdict := classifyWatchCheck(class, 1, true); verdict.Action != watchCheckRetry {
			t.Fatalf("class %q action = %q, want retry", class, verdict.Action)
		}
	}
}

// An ordinary non-zero exit stays observable: status CLIs use non-zero exits
// while an external operation is still converging, and parking those would break
// normal polling.
func TestClassifyWatchCheckKeepsOrdinaryExitsObservable(t *testing.T) {
	if verdict := classifyWatchCheck("", 0, false); verdict.Action != watchCheckObserve {
		t.Fatalf("clean check action = %q, want observe", verdict.Action)
	}
	if verdict := classifyWatchCheck("unknown", 1, true); verdict.Action != watchCheckObserve {
		t.Fatalf("unknown class action = %q, want observe", verdict.Action)
	}
}

func TestWatchCheckSignatureDistinguishesFailures(t *testing.T) {
	same := watchCheckSignature("credential_state_readonly", "bwrap: Can't mkdir parents\n")
	if same != watchCheckSignature("credential_state_readonly", "bwrap: Can't mkdir parents") {
		t.Fatal("trailing whitespace must not change the signature")
	}
	if same == watchCheckSignature("credential_state_readonly", "different output") {
		t.Fatal("different output must change the signature")
	}
	if same == watchCheckSignature("timeout", "bwrap: Can't mkdir parents\n") {
		t.Fatal("different class must change the signature")
	}
}

// The structured reason must survive into every downstream surface, because the
// live damage was a finalization run writing "build failed" into a release
// record on the strength of a check that never reached the build.
func TestWatchCheckDefectRecognizesStructuredReasons(t *testing.T) {
	for _, reason := range []string{
		watchReasonBlockedEnvironment,
		watchReasonInvalidCheck,
		watchReasonEnvironmentChanged,
		watchReasonRepeatedFailure,
	} {
		recorded := parkedWatchReason(reason, "detail here", "evidence here")
		got, ok := watchCheckDefect(recorded)
		if !ok || got != reason {
			t.Fatalf("reason %q was not recognized in %q", reason, recorded)
		}
		if !strings.Contains(recorded, "must not be treated as failed") {
			t.Fatalf("recorded reason lacks the do-not-conclude warning: %q", recorded)
		}
	}
	if _, ok := watchCheckDefect("FAILURE\n"); ok {
		t.Fatal("an observed business failure must not look like a check defect")
	}
	if _, ok := watchCheckDefect(""); ok {
		t.Fatal("an empty reason must not look like a check defect")
	}
}

func TestExternalWatchNoticeReportsBlockedChecksAsBlocked(t *testing.T) {
	blocked := control.ExternalWatch{
		ID:        "watch_ad92afb7",
		Status:    control.ExternalWatchBlocked,
		LastError: parkedWatchReason(watchReasonBlockedEnvironment, "the check's tool could not write its own state directory", "bwrap: ..."),
	}
	notice := externalWatchNotice(blocked, "waiting_finalization")
	if !strings.Contains(notice, "watch_ad92afb7 blocked:") {
		t.Fatalf("blocked notice = %q", notice)
	}
	if strings.Contains(notice, "status: failed") {
		t.Fatalf("a blocked check must not read as a failed operation: %q", notice)
	}

	observed := control.ExternalWatch{ID: "watch_1", Status: control.ExternalWatchFailed, LastError: "FAILURE"}
	if got := externalWatchNotice(observed, "waiting_finalization"); got != "Watcher watch_1 | status: failed | task: waiting_finalization" {
		t.Fatalf("observed failure notice = %q", got)
	}
}

func TestExternalWatchFinalizationContentRefusesVerdictForBlockedCheck(t *testing.T) {
	watch := control.ExternalWatch{
		ID:          "watch_1",
		Description: "Cloud Build reruns",
		Status:      control.ExternalWatchBlocked,
		LastOutput:  "CHECK_ERROR\tCHECK_ERROR",
		LastError:   parkedWatchReason(watchReasonBlockedEnvironment, "no usable credential", ""),
	}
	content := externalWatchFinalizationContent(watch, "summary")
	if !strings.Contains(content, "real state is unknown") {
		t.Fatalf("finalization prompt does not mark the state unknown: %q", content)
	}
	if !strings.Contains(content, "waiting_user") {
		t.Fatalf("finalization prompt must route a blocked check to a human: %q", content)
	}
	if strings.Contains(content, "finish as failed") {
		t.Fatalf("finalization prompt still offers a business verdict: %q", content)
	}
}

func TestExternalWatchOutcomeSeparatesCheckDefectFromFailure(t *testing.T) {
	watch := control.ExternalWatch{Description: "Cloud Build reruns"}
	blockedReason := parkedWatchReason(watchReasonBlockedEnvironment, "no usable credential", "")
	summary, next := externalWatchOutcome(watch, control.ExternalWatchBlocked, "CHECK_ERROR", blockedReason)
	if !strings.Contains(summary, "watcher for Cloud Build reruns was stopped") {
		t.Fatalf("blocked summary = %q", summary)
	}
	if strings.Contains(summary, "reported a failure") {
		t.Fatalf("blocked summary claims a business failure: %q", summary)
	}
	if len(next) == 0 || !strings.Contains(strings.Join(next, " "), "not record the external operation") {
		t.Fatalf("blocked next steps = %v", next)
	}

	summary, _ = externalWatchOutcome(watch, control.ExternalWatchFailed, "FAILURE", "exit status 1")
	if !strings.Contains(summary, "reported a failure") {
		t.Fatalf("observed failure summary = %q", summary)
	}
}
