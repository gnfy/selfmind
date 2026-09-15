package httpapi

import (
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
	"strings"
	"testing"
)

func TestWatchContinuationChecksCriteriaWithoutGrantingAuthority(t *testing.T) {
	w := control.ExternalWatch{ID: "watch", Status: control.ExternalWatchFailed, PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: control.ExternalWatchContinuationReceiptVersion}}
	if externalWatchContinuationProfile(w) != "" {
		t.Fatal("new continuation is file-only")
	}
	prompt := externalWatchFinalizationContent(w, "observed failure")
	for _, want := range []string{"acceptance criteria", "normal tools and current approval policy", "grants no new permissions", "Do not repeat completed effects", "Use verify"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q", want)
		}
	}
	result := reconcileExternalWatchOutcome(api.RunOutcome{Status: "blocked", CompletionReason: "environment", Resumable: true}, &w)
	if result.Status != "blocked" || !result.Resumable {
		t.Fatalf("model's remaining blocker overwritten: %+v", result)
	}
	w.PreflightReceipt.Version = 1
	if externalWatchContinuationProfile(w) != tools.ExecutionProfileWatchFinalization || !strings.Contains(externalWatchFinalizationContent(w, "failure"), "file tools only") {
		t.Fatal("historical watch gained capabilities")
	}
}

func TestNewObservationReceiptRejectsDocumentsAndPartialMatches(t *testing.T) {
	w := control.ExternalWatch{SpecVersion: 1, SuccessPattern: "SUCCESS", FailurePattern: "FAILED", PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: control.ExternalWatchContinuationReceiptVersion}}
	for _, out := range []string{`{"status":"WORKING","steps":[{"status":"SUCCESS"}]}`, "NOT_SUCCESS", "WORKING\nSUCCESS"} {
		if got := classifyExternalWatchOutput(w, out, 0); got != "" {
			t.Fatalf("false success for %s: %s", out, got)
		}
	}
	if got := classifyExternalWatchOutput(w, "SUCCESS\n", 0); got != control.ExternalWatchSucceeded {
		t.Fatal(got)
	}
	if got := classifyExternalWatchOutput(w, "SUCCESS", 1); got != "" {
		t.Fatal("failed check accepted")
	}
}
