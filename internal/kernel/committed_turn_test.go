package kernel

import (
	"context"
	"errors"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type acknowledgedTurnBackend struct {
	budgetClosureBackend
	committed bool
	calls     int
}

func (b *acknowledgedTurnBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	b.calls++
	if name == "finish_run" {
		b.committed = true
		return `{"status":"done","final_response":"Durable answer."}`, nil
	}
	return "unexpected", nil
}
func (b *acknowledgedTurnBackend) CommittedTurn() (string, string, bool) {
	return "done", "Durable answer.", b.committed
}

func TestAcknowledgedCommitSurvivesLaterLedgerFailureWithoutNewModelOrEffects(t *testing.T) {
	backend := &acknowledgedTurnBackend{}
	provider := &explicitFinishProvider{boundaryProvider: boundaryProvider{responses: []llm.ChatResponse{{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","final_response":"Durable answer."}`}}}}}}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 4, 1, nil)
	ctx := WithToolInvocationScope(context.Background(), ToolInvocationScope{RunID: "current", ControlTenantID: "default"})
	ctx = WithToolLedger(ctx, &resultFaultLedger{outcomeErr: errors.New("injected post-commit ledger failure")})
	ctx = WithCommittedTurnReader(ctx, backend)
	answer, _, err := agent.RunConversation(ctx, "person", "cli", "finish the observed work")
	if err != nil || answer != "Durable answer." || len(provider.requests) != 1 || backend.calls != 1 {
		t.Fatalf("committed result changed or work repeated: answer=%q err=%v requests=%d calls=%d", answer, err, len(provider.requests), backend.calls)
	}
	result := agent.executeSingleToolCall(ctx, "person", nil, 0, llm.ToolCall{ID: "late", Function: "terminal", Args: `{"command":"unexpected effect"}`})
	if result.success || backend.calls != 1 {
		t.Fatal("post-commit action was dispatched")
	}
	if _, _, ok := committedTurn(ForkDelegationContext(ctx)); ok {
		t.Fatal("delegated context inherited parent completion authority")
	}
}

func TestFinalBodyCannotCommitAlongsideUnobservedWork(t *testing.T) {
	b := &acknowledgedTurnBackend{}
	a := &Agent{backend: b}
	ctx := WithCommittedTurnReader(context.Background(), b)
	calls := []llm.ToolCall{{ID: "work", Function: "read_file", Args: `{"path":"input.txt"}`}, {ID: "finish", Function: "finish_run", Args: `{"status":"done","final_response":"Claimed result"}`}}
	results := a.executePlanAwareToolCalls(ctx, "person", nil, calls, nil, func([]llm.ToolCall, []toolExecutionResult) {})
	if b.calls != 0 || b.committed || len(results) != 2 {
		t.Fatalf("mixed completion dispatched: calls=%d results=%+v", b.calls, results)
	}
	for _, result := range results {
		if !result.heldBeforeDispatch || result.effectState != "not_dispatched" {
			t.Fatal("mixed batch lost non-dispatch evidence")
		}
	}
}
