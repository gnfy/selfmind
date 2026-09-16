package kernel

import "testing"

// humanDecisionError mirrors the typed contract the tools layer attaches to a
// parked approval or a rejection. Kernel must not import concrete tools, so
// the structural interfaces are exercised with a local double.
type humanDecisionError struct{ category, code, effect string }

func (e humanDecisionError) Error() string              { return "operation rejected: no" }
func (e humanDecisionError) ToolErrorCode() string      { return e.code }
func (e humanDecisionError) ToolErrorCategory() string  { return e.category }
func (e humanDecisionError) ModelSafeMessage() string   { return e.Error() }
func (e humanDecisionError) ToolRecoveryHint() string   { return "do not retry" }
func (e humanDecisionError) ToolFailurePhase() string   { return "authorization" }
func (e humanDecisionError) ToolRetryability() string   { return "user_decision" }
func (e humanDecisionError) ToolEffectState() string    { return e.effect }
func (e humanDecisionError) ToolStateChanged() bool     { return false }
func (e humanDecisionError) ToolAlternatives() []string { return nil }

// A human wait or a human decision reaches events under its own category and,
// because nothing was dispatched, never consumes the recovery policy's one
// correction for the plan step (observed live: a parked approval was recorded
// as error_category unknown and counted as a failed strategy).
func TestPackagedDecisionKeepsCategoryAndDoesNotCountAsStrategyFailure(t *testing.T) {
	for _, tc := range []struct{ category, code string }{
		{"human_wait", "approval_parked"},
		{"rejected", "approval_rejected"},
	} {
		err := humanDecisionError{category: tc.category, code: tc.code, effect: "not_dispatched"}
		packaged := packageToolErrorWithMetadata("terminal", err, ToolExecutionMetadata{})
		if packaged.ErrorCategory != tc.category || packaged.ErrorCode != tc.code || packaged.EffectState != "not_dispatched" {
			t.Fatalf("%s: packaged category=%q code=%q effect=%q", tc.category, packaged.ErrorCategory, packaged.ErrorCode, packaged.EffectState)
		}
		policy := NewStrategyRecoveryPolicy()
		attempt := RecoveryAttempt{ToolName: "terminal", PlanVersion: 1, PlanStepID: "step-1", InputSignature: "sig", TargetHash: "target", Strategy: "mutate"}
		for i := 0; i < 3; i++ {
			policy.RecordFailure(RecoveryFailure{Attempt: attempt, ErrorCode: packaged.ErrorCode, FailureClass: packaged.ErrorCategory,
				Retryability: packaged.Retryability, EffectState: packaged.EffectState})
		}
		if err := policy.BeforeDispatch(attempt); err != nil {
			t.Fatalf("%s: a human decision must not lock out the same command once it is allowed: %v", tc.category, err)
		}
	}
}
