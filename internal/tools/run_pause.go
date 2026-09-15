package tools

// runPauseError is a typed control-plane refusal of further work in this Run.
// It is distinct from an ordinary tool failure that Main can diagnose. Kernel
// consumes the structural interface; external result text cannot request it.
type runPauseError struct {
	cause           error
	reason, message string
	needApproval    bool
}

func (e *runPauseError) Error() string { return e.cause.Error() }
func (e *runPauseError) Unwrap() error { return e.cause }
func (e *runPauseError) ToolRunPause() (string, string, bool) {
	return e.reason, e.message, e.needApproval
}
