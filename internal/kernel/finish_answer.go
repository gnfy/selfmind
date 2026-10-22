package kernel

import (
	"encoding/json"
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

// acceptedFinishResponse reads only an accepted receipt, never inferred prose
// beside an action. Protocols without native assistant phases can use the
// same explicit body contract as protocols that label final_answer.
func acceptedFinishResponse(calls []llm.ToolCall, results []toolExecutionResult) string {
	for i, call := range calls {
		if strings.TrimSpace(call.Function) != "finish_run" || i >= len(results) {
			continue
		}
		var receipt struct {
			FinalResponse string `json:"final_response"`
		}
		if json.Unmarshal([]byte(results[i].rawResult), &receipt) != nil {
			return ""
		}
		body := strings.TrimSpace(receipt.FinalResponse)
		if acceptedFinishAnswer(calls, results, body) {
			return body
		}
	}
	return ""
}
