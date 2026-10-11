package kernel

import (
	"strings"

	"selfmind/internal/kernel/llm"
)

// A final prose label is not completion authority. Every bookkeeping receipt
// must be accepted, including finish_run, before its answer can close the loop.
func acceptedFinishAnswer(calls []llm.ToolCall, results []toolExecutionResult, prose string) bool {
	if strings.TrimSpace(prose) == "" || !onlyBookkeepingCalls(calls) || len(calls) != len(results) {
		return false
	}
	finished := false
	for i, call := range calls {
		if !results[i].success {
			return false
		}
		if _, ok := finishRunStatusFromToolCall(call); ok {
			finished = true
		}
	}
	return finished
}
