package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Registration preflight for a durable watch.
//
// A durable check is the one execution path with no host escape hatch and no
// model in the loop, so an environment or command defect there is terminal: the
// daemon can only repeat it. Both 2026-07-30 failures were of that shape, and
// both were visible on the very first run of the check.
//
// The four outcomes are deliberately asymmetric:
//
//	observed success   → nothing to watch; report it and register nothing.
//	observed failure   → return an ERROR, not a verdict. A check that reports a
//	                     terminal failure on its first run is the ambiguous case
//	                     (the live GCP check printed its own BUILD_FAILED while
//	                     both builds had succeeded), and the agent is still in
//	                     its turn and able to verify. The daemon must never write
//	                     an unattended failure into a release record on evidence
//	                     this thin.
//	check defect       → return an ERROR with the typed class, so the model fixes
//	                     the command or the environment now.
//	non-terminal       → register the watch.
const preflightMaxTimeout = 30 * time.Second

// preflightExternalWatch returns a non-empty tool reply when the watch must NOT
// be registered but the outcome is already known and benign (observed success).
// Anything the agent must fix comes back as an error.
func preflightExternalWatch(
	args map[string]interface{},
	command, cwd, successPattern, failurePattern string,
	commandTimeoutSeconds int,
) (string, error) {
	timeout := time.Duration(commandTimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > preflightMaxTimeout {
		timeout = preflightMaxTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	result, runErr := Execute(ctx, ExecutionRequest{
		ToolName:       "watch_external",
		Payload:        command,
		Shell:          true,
		CWD:            cwd,
		WorkspaceRoots: []string{cwd},
		Sandbox:        SandboxAuto,
		NetworkShared:  true,
		Timeout:        timeout,
		ToolProfile:    ToolProfile{Class: ToolExecutionStandard, MaxTimeout: timeout, HeartbeatInterval: time.Second},
	}, args)
	output := strings.TrimSpace(RedactSensitive(result.Output))

	// L0/L1: the check could not run, or is not a valid check. The typed class is
	// carried through verbatim so the model reads the same diagnosis a foreground
	// command would get, instead of a watcher-specific paraphrase.
	if class := strings.TrimSpace(result.FailureClass); class != "" && preflightBlockingClass(class) {
		return "", fmt.Errorf(
			"watch not registered: its first check failed with %s and repeating it in the background cannot fix that. "+
				"Detail: %s\nFix the check environment or the command, then register the watch again. "+
				"A daemon-owned check has no approval or host fallback, so it must work on the first try",
			class, truncatePreflight(firstNonEmptyPreflight(output, errText(runErr))))
	}

	// L2/L3: the check ran. Only a clean exit may declare success.
	if result.ExitCode == 0 && preflightMatches(successPattern, output) {
		return fmt.Sprintf(
			"Watch not registered: the check already reports the success condition, so there is nothing to wait for. "+
				"Observed output: %s\nTreat the operation as complete and finish the task normally.",
			truncatePreflight(output)), nil
	}
	if failurePattern != "" && preflightMatches(failurePattern, output) {
		return "", fmt.Errorf(
			"watch not registered: its first check already matches the failure pattern. "+
				"Observed output: %s\nThis is the case a background watcher must never decide alone — the check may be "+
				"reporting its own inability to query rather than a real failure. Verify the external state directly now, "+
				"while you can still inspect logs and credentials, before recording any outcome",
			truncatePreflight(output))
	}
	return "", nil
}

// preflightBlockingClass reports whether a failure class means "registering this
// watch cannot help". It mirrors the daemon-side park policy: environment and
// check-definition failures are terminal, transient ones are not.
func preflightBlockingClass(class string) bool {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "credential_state_readonly", "sandbox_fs_denied", "permission",
		"credential_missing", "credential_expired", "auth", "environment",
		"syntax", "not_found":
		return true
	default:
		// timeout / network / unknown: a status check may legitimately be slow or
		// exit non-zero while the external operation converges.
		return false
	}
}

func preflightMatches(pattern, output string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n"))
	if re.MatchString(normalized) {
		return true
	}
	for _, line := range strings.Split(normalized, "\n") {
		if line = strings.TrimSpace(line); line != "" && re.MatchString(line) {
			return true
		}
	}
	return false
}

func truncatePreflight(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " | "))
	if len(value) > 400 {
		return value[:400] + "…"
	}
	if value == "" {
		return "(no output)"
	}
	return value
}

func firstNonEmptyPreflight(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return RedactSensitive(err.Error())
}
