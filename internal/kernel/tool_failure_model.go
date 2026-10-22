package kernel

import "strings"

// toolFailureModelContent projects existing runtime facts, not another recovery
// protocol. Raw and typed envelope fields remain authoritative for policy and
// diagnostics. Only the framework error is simplified; captured output is not.
func toolFailureModelContent(name, message, hint string, env ToolResultEnvelope, userRejected bool) string {
	var parts []string
	// These constraints precede details so bounded output cannot hide them.
	if userRejected || env.Retryability == "user_decision" {
		parts = append(parts, "This operation was declined. Do NOT retry it or a variant in this turn.")
	}
	if env.EffectState == "unknown" {
		parts = append(parts, "Execution effects are uncertain. Observe their state before retrying.")
	} else if env.Retryability == "after_observation" {
		parts = append(parts, "Observe current state before another attempt.")
	}
	if env.EffectState == "not_dispatched" {
		parts = append(parts, "Not executed.")
	}
	if env.StateChanged {
		parts = append(parts, "State changed before this error.")
	}
	body := strings.TrimSpace(message)
	parts = append(parts, "Error executing "+nonEmpty(name, "tool")+": "+nonEmpty(body, "This attempt could not complete."))
	if hint != "" && !strings.Contains(body, hint) {
		parts = append(parts, hint)
	}
	if len(env.Alternatives) > 0 {
		choices := make([]string, 0, len(env.Alternatives))
		for _, alternative := range env.Alternatives {
			choices = append(choices, strings.ReplaceAll(alternative, "_", " "))
		}
		parts = append(parts, "Other supported approaches: "+strings.Join(choices, "; ")+".")
	}
	return strings.Join(parts, "\n")
}

// Legacy built-ins append classification to their error string. External error
// text and captured output are data and never pass through this projection.
func plainToolFailureMessage(message string) string {
	var lines []string
	for _, line := range strings.Split(message, "\n") {
		key := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(key, "error_class:") || strings.HasPrefix(key, "error_code:") ||
			strings.HasPrefix(key, "failure_phase:") || strings.HasPrefix(key, "retryability:") ||
			strings.HasPrefix(key, "effect_state:") || strings.HasPrefix(key, "alternatives:") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
