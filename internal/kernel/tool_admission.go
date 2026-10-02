package kernel

import "selfmind/internal/kernel/llm"

type refusedToolCall struct {
	call llm.ToolCall
	err  toolAdmissionError
}

// Admission changes dispatch, never the fact that the model requested a call.
// Keep rejected calls paired with typed results, including calls excluded by
// the reduced completion schema. Neither a dispatch claim nor budget is spent.
func admitToolCalls(requested []llm.ToolCall, strategy TaskStrategy, used int, counts map[string]int) ([]llm.ToolCall, []refusedToolCall, int, int) {
	allowed, budget := filterToolCallsByStrategyAndBudget(requested, strategy, used)
	allowed, caps := filterToolCallsByLifecycleCaps(allowed, counts)
	admitted := make(map[string]bool, len(allowed))
	for _, call := range allowed {
		admitted[call.ID] = true
	}
	var refused []refusedToolCall
	for _, call := range requested {
		if admitted[call.ID] {
			continue
		}
		code := "tool_budget_exhausted"
		if !strategy.AllowsTool(call.Function) {
			atBudgetBoundary := used >= strategy.MaxActionTools ||
				(strategy.CompletionReserve > 0 && used >= strategy.ActionToolBudgetLimit-strategy.CompletionReserve)
			if !atBudgetBoundary {
				code = "tool_not_available"
				budget--
			}
		} else if cap := lifecycleToolCap(call.Function); cap > 0 && counts[call.Function] >= cap {
			code = "tool_attempt_limit"
		}
		refused = append(refused, refusedToolCall{call: call, err: toolAdmissionError{code: code}})
	}
	return allowed, refused, budget + caps, caps
}

func requestedCallsWithRefusals(allowed []llm.ToolCall, refused []refusedToolCall) []llm.ToolCall {
	out := append([]llm.ToolCall(nil), allowed...)
	for _, rejected := range refused {
		out = append(out, rejected.call)
	}
	return out
}

type toolAdmissionError struct{ code string }

func (e toolAdmissionError) Error() string {
	switch e.code {
	case "tool_not_available":
		return "Tool call was not executed: this tool is unavailable in the current turn phase or strategy."
	case "tool_attempt_limit":
		return "Tool call was not executed: this lifecycle tool reached its bounded attempt limit."
	default:
		return "Tool call was not executed: the action budget is exhausted or reserved for verification."
	}
}
func (e toolAdmissionError) ModelSafeMessage() string { return e.Error() }
func (e toolAdmissionError) ToolErrorCode() string    { return e.code }
func (toolAdmissionError) ToolErrorCategory() string  { return "policy" }
func (toolAdmissionError) ToolRecoveryHint() string {
	return "Use the currently exposed tools to record an honest outcome from existing evidence. Do not claim the refused action happened. If necessary work remains, preserve it as unfinished."
}
func (toolAdmissionError) ToolFailurePhase() string   { return "admission" }
func (toolAdmissionError) ToolRetryability() string   { return "after_policy_change" }
func (toolAdmissionError) ToolEffectState() string    { return "not_dispatched" }
func (toolAdmissionError) ToolStateChanged() bool     { return false }
func (toolAdmissionError) ToolAlternatives() []string { return nil }
