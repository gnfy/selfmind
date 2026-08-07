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

type externalWatchPreflightPatterns struct {
	Success         string
	Failure         string
	Target          string
	TerminalSuccess string
	TerminalFailure string
}

// preflightExternalWatch returns a non-empty tool reply when the watch must NOT
// be registered but the outcome is already known and benign (observed success).
// Anything the agent must fix comes back as an error.
func preflightExternalWatch(
	args map[string]interface{},
	command, cwd, successPattern, failurePattern string,
	commandTimeoutSeconds int,
) (string, error) {
	return preflightExternalWatchPatterns(args, command, cwd, externalWatchPreflightPatterns{
		Success: successPattern, Failure: failurePattern,
	}, commandTimeoutSeconds)
}

func preflightExternalWatchPatterns(
	args map[string]interface{},
	command, cwd string,
	patterns externalWatchPreflightPatterns,
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
	if strings.EqualFold(strings.TrimSpace(result.FailureClass), "timeout") {
		return "", fmt.Errorf(
			"watch not registered: its first check exceeded the %s registration budget. "+
				"A durable watcher repeats the same check unattended, so a command that is already too slow here would monopolize every poll. "+
				"Split aggregate or multi-target checks into independent bounded checks, or replace the script with one status query that completes within the budget. Detail: %s",
			timeout, truncatePreflight(firstNonEmptyPreflight(output, errText(runErr))))
	}

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

	// A durable watcher has no model in its polling loop. Registration therefore
	// requires one clean execution now; a non-zero/failed first check is evidence
	// that the frozen command is not yet safe to repeat unattended. This also
	// prevents an unknown script failure from being mistaken for a pending state.
	if runErr != nil || result.ExitCode != 0 {
		class := strings.TrimSpace(result.FailureClass)
		if class == "" {
			class = ClassifyToolError("watch_external", runErr, output)
		}
		return "", fmt.Errorf(
			"watch not registered: its first check did not complete successfully (exit=%d, class=%s). "+
				"Detail: %s\nFix or retry the foreground check until it exits 0, then register the durable watch",
			result.ExitCode, firstNonEmptyPreflight(class, "unknown"),
			truncatePreflight(firstNonEmptyPreflight(output, errText(runErr))))
	}
	if output == "" {
		return "", fmt.Errorf(
			"watch not registered: its first check exited 0 but produced no observable state. " +
				"Make the check print one bounded status value that the registered patterns can evaluate")
	}
	if class := ClassifyToolError("watch_external", nil, output); class == "check_definition" {
		return "", fmt.Errorf(
			"watch not registered: its first check output shows a check-definition failure. Detail: %s\n%s",
			truncatePreflight(output), errorClassHints[class])
	}

	// V2 terminal failures are authoritative and evaluated before success or a
	// desired handoff state. V1 keeps its historical success-first behavior.
	if patterns.TerminalFailure != "" && preflightMatches(patterns.TerminalFailure, output) {
		return "", preflightObservedFailure(output)
	}
	if result.ExitCode == 0 && preflightMatches(patterns.TerminalSuccess, output) {
		return preflightObservedSuccess(output), nil
	}
	if result.ExitCode == 0 && preflightMatches(patterns.Target, output) {
		return fmt.Sprintf(
			"Watch not registered: the check already reports the desired handoff state. Observed output: %s\nContinue from that state in the current run.",
			truncatePreflight(output)), nil
	}

	// L2/L3 V1 compatibility: only a clean exit may declare success.
	if result.ExitCode == 0 && preflightMatches(patterns.Success, output) {
		return preflightObservedSuccess(output), nil
	}
	if patterns.Failure != "" && preflightMatches(patterns.Failure, output) {
		return "", preflightObservedFailure(output)
	}
	return "", nil
}

func preflightObservedSuccess(output string) string {
	return fmt.Sprintf(
		"Watch not registered: the check already reports the success condition, so there is nothing to wait for. "+
			"Observed output: %s\nTreat the operation as complete and finish the task normally.",
		truncatePreflight(output))
}

func preflightObservedFailure(output string) error {
	return fmt.Errorf(
		"watch not registered: its first check already matches the failure pattern. "+
			"Observed output: %s\nThis is the case a background watcher must never decide alone — the check may be "+
			"reporting its own inability to query rather than a real failure. Verify the external state directly now, "+
			"while you can still inspect logs and credentials, before recording any outcome",
		truncatePreflight(output))
}

// preflightBlockingClass reports whether a failure class means "registering this
// watch cannot help". It mirrors the daemon-side park policy: environment and
// check-definition failures are terminal, transient ones are not.
func preflightBlockingClass(class string) bool {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "credential_state_readonly", "sandbox_fs_denied", "permission",
		"credential_missing", "credential_expired", "auth", "environment",
		"syntax", "not_found", "timeout", "check_definition":
		return true
	default:
		// Non-zero network/unknown failures are rejected by the clean-exit
		// contract above. This helper identifies typed failures that are terminal
		// even before that generic gate.
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
