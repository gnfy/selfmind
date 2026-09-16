package tools

import "errors"

// Redaction changes presentation, never the typed failure/recovery contract or
// lifecycle error identity. Unwrap retains cancellation and human-wait signals.
type redactedCause struct {
	error
	text string
}

func (e redactedCause) Error() string { return e.text }
func (e redactedCause) Unwrap() error { return e.error }

func redactToolFailure(err error) error {
	if err == nil {
		return nil
	}
	text := RedactSensitive(err.Error())
	out := &stableToolError{cause: redactedCause{error: err, text: text}, safeMessage: text}
	changed := text != err.Error()
	var stable interface {
		ToolErrorCode() string
		ToolErrorCategory() string
		ModelSafeMessage() string
		ToolRecoveryHint() string
	}
	if errors.As(err, &stable) {
		out.code = RedactSensitive(stable.ToolErrorCode())
		out.category = RedactSensitive(stable.ToolErrorCategory())
		out.safeMessage = RedactSensitive(stable.ModelSafeMessage())
		out.recoveryHint = RedactSensitive(stable.ToolRecoveryHint())
		changed = changed || out.code != stable.ToolErrorCode() || out.category != stable.ToolErrorCategory() || out.safeMessage != stable.ModelSafeMessage() || out.recoveryHint != stable.ToolRecoveryHint()
	}
	var recovery interface {
		ToolFailurePhase() string
		ToolRetryability() string
		ToolEffectState() string
		ToolStateChanged() bool
		ToolAlternatives() []string
	}
	if errors.As(err, &recovery) {
		out.failurePhase = RedactSensitive(recovery.ToolFailurePhase())
		out.retryability = RedactSensitive(recovery.ToolRetryability())
		out.effectState = RedactSensitive(recovery.ToolEffectState())
		out.stateChanged = recovery.ToolStateChanged()
		changed = changed || out.failurePhase != recovery.ToolFailurePhase() || out.retryability != recovery.ToolRetryability() || out.effectState != recovery.ToolEffectState()
		for _, item := range recovery.ToolAlternatives() {
			masked := RedactSensitive(item)
			out.alternatives = append(out.alternatives, masked)
			changed = changed || masked != item
		}
	}
	if !changed {
		return err
	}
	return out
}
