package tools

// Human waits and human decisions stop a tool call, but they are outcomes,
// not tool failures. They travel through the same typed contract as other
// refusals so events carry their real category, the model sees why it
// stopped, and the recovery policy never counts them as a failed strategy
// (effect_state not_dispatched).

const (
	// toolErrorCategoryHumanWait marks a call that stopped because nobody has
	// answered yet; a later decision resumes the work.
	toolErrorCategoryHumanWait = "human_wait"
	// toolErrorCategoryRejected marks a decision that refused the call: a
	// person, safety triage, or a capability policy. It is never retried.
	toolErrorCategoryRejected = "rejected"
)

const (
	rejectionCodeApproval   = "approval_rejected"
	rejectionCodeTriage     = "triage_denied"
	rejectionCodeCapability = "capability_denied"
	rejectionCodeScope      = "approval_scope_rejected"
)

// approvalRejectedError keeps the "operation rejected" wording: kernel's
// isUserRejectionErr matches that prefix across the package boundary and
// replaces diagnose-and-retry guidance with the do-not-retry instruction.
type approvalRejectedError struct {
	text string
	code string
}

func rejectOperation(code, text string) error {
	return &approvalRejectedError{text: text, code: code}
}

func (e *approvalRejectedError) Error() string             { return e.text }
func (e *approvalRejectedError) ToolErrorCode() string     { return e.code }
func (e *approvalRejectedError) ToolErrorCategory() string { return toolErrorCategoryRejected }
func (e *approvalRejectedError) ModelSafeMessage() string  { return e.text }
func (e *approvalRejectedError) ToolRecoveryHint() string {
	return "This is a decision, not a failure. Do not retry this operation or a variant of it; report what was not done and continue only with genuinely different work, or finish."
}
func (e *approvalRejectedError) ToolFailurePhase() string   { return "authorization" }
func (e *approvalRejectedError) ToolRetryability() string   { return "user_decision" }
func (e *approvalRejectedError) ToolEffectState() string    { return "not_dispatched" }
func (e *approvalRejectedError) ToolStateChanged() bool     { return false }
func (e *approvalRejectedError) ToolAlternatives() []string { return nil }
