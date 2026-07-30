package httpapi

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Watcher check policy: which LAYER failed, and what that permits.
//
// A durable check has four layers, and they were previously collapsed into two
// regexes over one output string:
//
//	L0 environment  the sandbox/state/credential material could be prepared
//	L1 execution    the check process actually ran
//	L2 observation  the check could READ the external system's state
//	L3 business     the external operation is pending / succeeded / failed
//
// Both live defects were lower-layer failures rendered as L3 verdicts: a
// sandbox that could not be constructed became "still running" and was retried
// for two hours, and a check script that swallowed its own stderr turned
// "cannot query" into a matched failure_pattern, which reported a build as
// FAILED when both builds had in fact succeeded. Hence the invariant this file
// enforces: a lower-layer failure never reaches pattern matching, and never
// becomes a business verdict.
const (
	// watchCheckObserve means the check ran well enough that its output may be
	// matched against the declared patterns.
	watchCheckObserve = "observe"
	// watchCheckRetry means the failure is transient; check again on schedule.
	watchCheckRetry = "retry"
	// watchCheckPark means retrying cannot change the answer. The watch stops
	// and a human (or the agent) is told what to fix.
	watchCheckPark = "park"
)

// Structured reason prefixes. They are the machine-readable part of a parked
// watch's last_error, so the finalization prompt and the user-facing notice can
// state that the CHECK failed rather than the external operation.
const (
	watchReasonBlockedEnvironment = "blocked_environment"
	watchReasonInvalidCheck       = "invalid_check"
	watchReasonEnvironmentChanged = "environment_changed"
	watchReasonRepeatedFailure    = "repeated_failure"
)

// watchCheckVerdict is the layer decision for one executed check.
type watchCheckVerdict struct {
	Action string
	// Layer records where the failure was diagnosed, for events and diagnostics.
	Layer string
	// Reason is the structured prefix a parked watch records.
	Reason string
	// Detail is a short, non-secret explanation for the operator.
	Detail string
}

// watchCheckPolicy maps a typed failure class to a layer decision.
//
// The classes come from tools.ClassifyToolError — the SAME classifier the
// foreground tool path uses. There is deliberately no second marker table here:
// the watcher used to keep its own list of substrings, and the two disagreed
// precisely on the case that mattered ("read-only file system" was in one and
// not the other).
var watchCheckPolicy = map[string]watchCheckVerdict{
	// L0: the execution environment could not be prepared. Repeating the same
	// command with the same plan cannot produce a different result.
	"credential_state_readonly": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "the check's tool could not write its own state directory inside the sandbox"},
	"sandbox_fs_denied": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "the sandbox denied a write the check needs"},
	"permission": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "the check was denied permission for a path or operation"},
	"credential_missing": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "no usable credential is present for the check's tool"},
	"credential_expired": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "the credential the check needs has expired"},
	"auth": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "the external service rejected the check's credentials"},
	"environment": {Action: watchCheckPark, Layer: "environment", Reason: watchReasonBlockedEnvironment,
		Detail: "an interpreter, package, or variable the check needs is missing"},

	// L1: the check itself is defective. Retrying a broken command is noise.
	"syntax": {Action: watchCheckPark, Layer: "execution", Reason: watchReasonInvalidCheck,
		Detail: "the check command is not valid shell"},
	"not_found": {Action: watchCheckPark, Layer: "execution", Reason: watchReasonInvalidCheck,
		Detail: "the check's executable or path does not exist"},

	// Transient: check again on schedule.
	"timeout": {Action: watchCheckRetry, Layer: "execution"},
	"network": {Action: watchCheckRetry, Layer: "execution"},
}

// classifyWatchCheck decides what one executed check permits.
//
// An "unknown" class with a non-zero exit stays observable on purpose: many
// status CLIs exit non-zero while an external operation is still converging,
// and turning that into a parked watch would break the normal polling case.
func classifyWatchCheck(failureClass string, exitCode int, failed bool) watchCheckVerdict {
	class := strings.ToLower(strings.TrimSpace(failureClass))
	if verdict, ok := watchCheckPolicy[class]; ok {
		return verdict
	}
	if failed || exitCode != 0 {
		return watchCheckVerdict{Action: watchCheckObserve, Layer: "observation"}
	}
	return watchCheckVerdict{Action: watchCheckObserve, Layer: "observation"}
}

// watchCheckSignature identifies "the same failure again" for the circuit
// breaker: the typed class plus a hash of the output. A watch may legitimately
// fail the same way twice (a slow external system), but the same environment
// failure repeated is not new evidence — it is the two-hour retry loop.
func watchCheckSignature(failureClass, output string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(output)))
	return fmt.Sprintf("%s:%x", strings.ToLower(strings.TrimSpace(failureClass)), sum[:8])
}

// watchRepeatedFailureLimit is how many identical consecutive failures a watch
// may accumulate before it parks. Three is enough to absorb a genuine blip and
// short enough that the operator hears about a stuck watch in ~90 seconds at
// the default interval, instead of at the two-hour deadline.
const watchRepeatedFailureLimit = 3

// parkedWatchReason renders the structured reason recorded on a parked watch.
func parkedWatchReason(reason, detail, evidence string) string {
	parts := []string{reason + ": " + strings.TrimSpace(detail)}
	if evidence = strings.TrimSpace(evidence); evidence != "" {
		parts = append(parts, "Evidence: "+truncate(toOneLine(evidence), 240))
	}
	parts = append(parts,
		"The external operation's state was NOT observed and must not be treated as failed. "+
			"Fix the check environment or command, then verify the external state directly.")
	return strings.Join(parts, " ")
}

// watchCheckDefect reports whether a watch's recorded reason is a check defect
// rather than an observed business outcome. The distinction has to survive all
// the way to the finalization prompt and the user notice: the damage in the
// live incident was an agent writing "build failed" into a release record on
// the strength of a check that never reached the build.
func watchCheckDefect(lastError string) (string, bool) {
	trimmed := strings.TrimSpace(lastError)
	for _, reason := range []string{
		watchReasonBlockedEnvironment,
		watchReasonInvalidCheck,
		watchReasonEnvironmentChanged,
		watchReasonRepeatedFailure,
	} {
		if strings.HasPrefix(trimmed, reason+":") {
			return reason, true
		}
	}
	return "", false
}
