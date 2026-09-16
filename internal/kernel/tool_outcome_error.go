package kernel

// The tool returned; only its durable outcome failed. Never represent this as
// a pre-dispatch refusal or permit a blind retry to repair a database write.
type toolOutcomePersistenceError struct{ cause error }

func (e *toolOutcomePersistenceError) Error() string {
	return e.ModelSafeMessage() + ": " + e.cause.Error()
}
func (e *toolOutcomePersistenceError) Unwrap() error { return e.cause }
func (*toolOutcomePersistenceError) ModelSafeMessage() string {
	return "The tool returned, but its durable outcome could not be recorded. External state is uncertain and must be verified before any retry."
}
func (*toolOutcomePersistenceError) ToolErrorCode() string     { return "tool_outcome_unrecorded" }
func (*toolOutcomePersistenceError) ToolErrorCategory() string { return "uncertain_effect" }
func (*toolOutcomePersistenceError) ToolRecoveryHint() string {
	return "Observe the actual result before retrying the action; retain the unfinished work and captured output."
}
func (*toolOutcomePersistenceError) ToolFailurePhase() string   { return "outcome_recording" }
func (*toolOutcomePersistenceError) ToolRetryability() string   { return "after_observation" }
func (*toolOutcomePersistenceError) ToolEffectState() string    { return "unknown" }
func (*toolOutcomePersistenceError) ToolStateChanged() bool     { return false }
func (*toolOutcomePersistenceError) ToolAlternatives() []string { return nil }
