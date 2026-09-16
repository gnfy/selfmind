package tools

import "errors"

// runPauseError is a typed run boundary: the tool call did not run and the
// Run must park on a human before any further dispatch, independently of the
// model reading the message. It is also a typed refusal for kernel's failure
// packaging, so the pause reaches events and the model as human_wait with
// effect_state not_dispatched, never as an unknown failure or a failed
// strategy (observed live: a parked approval recorded error_category unknown
// and was counted by the recovery policy). A typed cause, such as a
// stale-precondition pause, keeps its own code and category.
type runPauseError struct {
	cause           error
	reason, message string
	needApproval    bool
}

type typedToolFailure interface {
	ToolErrorCode() string
	ToolErrorCategory() string
	ModelSafeMessage() string
	ToolRecoveryHint() string
}

type typedToolRecovery interface {
	ToolFailurePhase() string
	ToolRetryability() string
	ToolEffectState() string
	ToolStateChanged() bool
	ToolAlternatives() []string
}

func (e *runPauseError) Error() string { return e.cause.Error() }
func (e *runPauseError) Unwrap() error { return e.cause }
func (e *runPauseError) ToolRunPause() (string, string, bool) {
	return e.reason, e.message, e.needApproval
}

func (e *runPauseError) typedCause() (typedToolFailure, bool) {
	var typed typedToolFailure
	if errors.As(e.cause, &typed) {
		return typed, true
	}
	return nil, false
}

func (e *runPauseError) recoveryCause() (typedToolRecovery, bool) {
	var typed typedToolRecovery
	if errors.As(e.cause, &typed) {
		return typed, true
	}
	return nil, false
}

func (e *runPauseError) ToolErrorCode() string {
	if typed, ok := e.typedCause(); ok {
		return typed.ToolErrorCode()
	}
	if e.needApproval {
		return "approval_parked"
	}
	return "run_paused"
}

func (e *runPauseError) ToolErrorCategory() string {
	if typed, ok := e.typedCause(); ok {
		return typed.ToolErrorCategory()
	}
	return toolErrorCategoryHumanWait
}

func (e *runPauseError) ModelSafeMessage() string {
	if typed, ok := e.typedCause(); ok {
		return typed.ModelSafeMessage()
	}
	return e.cause.Error()
}

func (e *runPauseError) ToolRecoveryHint() string {
	if typed, ok := e.typedCause(); ok {
		return typed.ToolRecoveryHint()
	}
	return "Nobody has answered yet. Do not retry or vary this action; finish with waiting_user and the pending decision resumes the work."
}

func (e *runPauseError) ToolFailurePhase() string {
	if typed, ok := e.recoveryCause(); ok {
		return typed.ToolFailurePhase()
	}
	return "authorization"
}

func (e *runPauseError) ToolRetryability() string {
	if typed, ok := e.recoveryCause(); ok {
		return typed.ToolRetryability()
	}
	return "after_human"
}

func (e *runPauseError) ToolEffectState() string {
	if typed, ok := e.recoveryCause(); ok {
		return typed.ToolEffectState()
	}
	return "not_dispatched"
}

func (e *runPauseError) ToolStateChanged() bool {
	if typed, ok := e.recoveryCause(); ok {
		return typed.ToolStateChanged()
	}
	return false
}

func (e *runPauseError) ToolAlternatives() []string {
	if typed, ok := e.recoveryCause(); ok {
		return typed.ToolAlternatives()
	}
	return nil
}
