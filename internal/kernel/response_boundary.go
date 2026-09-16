package kernel

// A syntactically valid call is not evidence of a complete model response.
// Keep this refusal distinct from a tool that ran and failed or an unknown
// external effect. No dispatch claim or tool budget is consumed.
type incompleteToolResponseError struct{}

func (incompleteToolResponseError) Error() string {
	return "Tool call was not executed because the model response reached its output limit."
}
func (e incompleteToolResponseError) ModelSafeMessage() string { return e.Error() }
func (incompleteToolResponseError) ToolErrorCode() string      { return "model_response_incomplete" }
func (incompleteToolResponseError) ToolErrorCategory() string  { return "protocol" }
func (incompleteToolResponseError) ToolRecoveryHint() string {
	return "Re-issue the still-needed call with complete arguments within the remaining turn budget."
}
func (incompleteToolResponseError) ToolFailurePhase() string   { return "response_validation" }
func (incompleteToolResponseError) ToolRetryability() string   { return "after_correction" }
func (incompleteToolResponseError) ToolEffectState() string    { return "not_dispatched" }
func (incompleteToolResponseError) ToolStateChanged() bool     { return false }
func (incompleteToolResponseError) ToolAlternatives() []string { return nil }
