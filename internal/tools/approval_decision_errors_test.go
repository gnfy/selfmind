package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type decisionFailure interface {
	ToolErrorCode() string
	ToolErrorCategory() string
	ModelSafeMessage() string
	ToolRecoveryHint() string
	ToolFailurePhase() string
	ToolRetryability() string
	ToolEffectState() string
	ToolStateChanged() bool
}

func decisionFacts(t *testing.T, err error) decisionFailure {
	t.Helper()
	var typed decisionFailure
	if !errors.As(err, &typed) {
		t.Fatalf("decision is not typed: %T %v", err, err)
	}
	return typed
}

// A parked approval used to reach kernel as an untyped error: tool.completed
// recorded error_category unknown and the recovery policy counted the human
// wait as a failed strategy. The pause keeps its run-boundary contract and
// now also says what it is.
func TestApprovalTimeoutPauseIsTypedHumanWait(t *testing.T) {
	asked := 0
	scope := ExecutionScope{
		TenantID: "tenant-p", PersonID: "person-p", TaskID: "task-1",
		ApprovalMode: ApprovalSmart, Grants: newFakeGrantStore(), Judge: &fakeJudge{reply: "ESCALATE"},
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			return ToolApprovalDecision{Approved: false, Outcome: ApprovalOutcomeTimedOut, Reason: "approval parked"}, nil
		},
	}
	ran, err := runSmart(t, scope, "person-p", "rm -rf build")
	if ran || err == nil || asked != 1 {
		t.Fatalf("timed-out approval must stop the call after one ask: ran=%v asked=%d err=%v", ran, asked, err)
	}
	var pause interface{ ToolRunPause() (string, string, bool) }
	if !errors.As(err, &pause) {
		t.Fatalf("timeout must still park the run: %T %v", err, err)
	}
	if reason, _, needApproval := pause.ToolRunPause(); reason != "waiting_user" || !needApproval {
		t.Fatalf("pause = %q needApproval=%v", reason, needApproval)
	}
	facts := decisionFacts(t, err)
	if facts.ToolErrorCategory() != toolErrorCategoryHumanWait || facts.ToolErrorCode() != "approval_parked" ||
		facts.ToolEffectState() != "not_dispatched" || facts.ToolFailurePhase() != "authorization" || facts.ToolStateChanged() {
		t.Fatalf("parked approval is not a typed human wait: code=%q category=%q effect=%q phase=%q",
			facts.ToolErrorCode(), facts.ToolErrorCategory(), facts.ToolEffectState(), facts.ToolFailurePhase())
	}
	if !strings.Contains(facts.ModelSafeMessage(), "approval parked") {
		t.Fatalf("the model must still see why it stopped: %q", facts.ModelSafeMessage())
	}
}

// A person's refusal is a decision. It keeps the exact "operation rejected"
// wording kernel matches and now also carries its category, so the event
// stream distinguishes it from a tool that ran and failed.
func TestHumanRejectionIsTypedDecision(t *testing.T) {
	scope := ExecutionScope{
		TenantID: "tenant-r", PersonID: "person-r", TaskID: "task-1",
		ApprovalMode: ApprovalSmart, Grants: newFakeGrantStore(), Judge: &fakeJudge{reply: "ESCALATE"},
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{Approved: false, ApprovalID: "apr-1", Reason: "not today"}, nil
		},
	}
	ran, err := runSmart(t, scope, "person-r", "rm -rf build")
	if ran || err == nil {
		t.Fatalf("rejected call must not run: ran=%v err=%v", ran, err)
	}
	if !strings.HasPrefix(err.Error(), "operation rejected: not today") {
		t.Fatalf("kernel prefix contract broken: %v", err)
	}
	facts := decisionFacts(t, err)
	if facts.ToolErrorCategory() != toolErrorCategoryRejected || facts.ToolErrorCode() != rejectionCodeApproval ||
		facts.ToolEffectState() != "not_dispatched" || facts.ToolRetryability() != "user_decision" {
		t.Fatalf("rejection is not a typed decision: code=%q category=%q effect=%q retry=%q",
			facts.ToolErrorCode(), facts.ToolErrorCategory(), facts.ToolEffectState(), facts.ToolRetryability())
	}
	if !strings.Contains(facts.ModelSafeMessage(), "not today") {
		t.Fatalf("the reason must reach the model: %q", facts.ModelSafeMessage())
	}
}

// A pause raised over an already typed cause keeps that cause's code and
// category; only an untyped pause defaults to the human-wait contract.
func TestRunPauseKeepsTypedCauseAndDefaultsToHumanWait(t *testing.T) {
	typed := &runPauseError{
		cause: newStableToolRecoveryError(errors.New("blocked"), "work_selection_blocked", "stale_precondition", "blocked", "review effects",
			"preparation", "after_user_input", "not_dispatched", false),
		reason: "work_selection_rejected", message: "m",
	}
	facts := decisionFacts(t, typed)
	if facts.ToolErrorCode() != "work_selection_blocked" || facts.ToolErrorCategory() != "stale_precondition" ||
		facts.ToolRetryability() != "after_user_input" || facts.ToolFailurePhase() != "preparation" {
		t.Fatalf("typed cause was overridden: code=%q category=%q retry=%q phase=%q",
			facts.ToolErrorCode(), facts.ToolErrorCategory(), facts.ToolRetryability(), facts.ToolFailurePhase())
	}
	facts = decisionFacts(t, &runPauseError{cause: errors.New("approval timed out"), reason: "waiting_user", needApproval: true})
	if facts.ToolErrorCategory() != toolErrorCategoryHumanWait || facts.ToolErrorCode() != "approval_parked" || facts.ToolRetryability() != "after_human" {
		t.Fatalf("untyped approval pause: code=%q category=%q retry=%q", facts.ToolErrorCode(), facts.ToolErrorCategory(), facts.ToolRetryability())
	}
	facts = decisionFacts(t, &runPauseError{cause: errors.New("paused"), reason: "waiting_user"})
	if facts.ToolErrorCode() != "run_paused" || facts.ToolErrorCategory() != toolErrorCategoryHumanWait {
		t.Fatalf("untyped pause without approval: code=%q category=%q", facts.ToolErrorCode(), facts.ToolErrorCategory())
	}
}
