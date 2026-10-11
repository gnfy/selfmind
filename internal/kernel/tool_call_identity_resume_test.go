package kernel

import (
	"context"
	"fmt"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type resumedIdentityLedger struct{ entries map[string]ToolLedgerEntry }

func (l *resumedIdentityLedger) ClaimDispatch(_ context.Context, e ToolLedgerEntry) (ToolDispatchDecision, error) {
	if old, ok := l.entries[e.ToolCallID]; ok {
		if old.ArgsHash != e.ArgsHash || old.ToolName != e.ToolName {
			return ToolDispatchDecision{}, fmt.Errorf("tool call id %s was already claimed with different arguments", e.ToolCallID)
		}
		return ToolDispatchDecision{Status: "completed"}, nil
	}
	l.entries[e.ToolCallID] = e
	return ToolDispatchDecision{Execute: true, Status: "started"}, nil
}
func (*resumedIdentityLedger) RecordOutcome(context.Context, string, string, bool) error { return nil }

type resumedIdentityBackend struct{ calls []string }

func (b *resumedIdentityBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name == "terminal" {
		b.calls = append(b.calls, fmt.Sprint(args["command"]))
		return "observed", nil
	}
	if name == "finish_run" {
		return `{"status":"done","summary":"checked"}`, nil
	}
	return "", fmt.Errorf("unexpected tool %s", name)
}
func (*resumedIdentityBackend) GetToolDefinitions() []map[string]interface{} {
	var defs []map[string]interface{}
	for _, n := range []string{"terminal", "finish_run"} {
		defs = append(defs, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": n, "description": n, "parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}})
	}
	return defs
}

type resumedIdentityProvider struct {
	mockLLMProvider
	calls int
	id    string
}

func (*resumedIdentityProvider) SupportsNativeTools() bool { return true }
func (p *resumedIdentityProvider) StreamChat(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	ch := make(chan llm.StreamEvent, 1)
	switch p.calls {
	case 1:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: p.id, Function: "terminal", Args: `{"command":"new check"}`}}}
	case 2:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}}
	default:
		ch <- llm.StreamEvent{Content: "Checked."}
	}
	close(ch)
	return ch, nil
}
func TestApprovalContinuationKeepsDistinctGeneratedCalls(t *testing.T) {
	for _, id := range []string{"", "provider-new-call"} {
		t.Run(fmt.Sprintf("native_id_%q", id), func(t *testing.T) {
			b := &resumedIdentityBackend{}
			p := &resumedIdentityProvider{id: id}
			a := NewAgent(memory.NewMemoryManager(&mockStorage{}), b, p, "helpful", 4, 1, nil)
			pending := llm.ToolCall{ID: "toolcall-0-0", Function: "terminal", Args: `{"command":"approved original"}`}
			ctx := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{RunID: "probe-child"})
			ctx = WithToolLedger(ctx, &resumedIdentityLedger{entries: map[string]ToolLedgerEntry{}})
			ctx = WithLoopResumeMessages(ctx, []llm.Message{{Role: "user", Content: "perform both checks"}, {Role: "assistant", ToolCalls: []llm.ToolCall{pending}}})
			ctx = WithHumanWaitResume(ctx, HumanWaitResume{ToolCallID: pending.ID})
			_, _, err := a.RunConversation(ctx, "test", "cli", "continue checks")
			if err != nil {
				t.Fatal(err)
			}
			if len(b.calls) != 2 || b.calls[0] != "approved original" || b.calls[1] != "new check" {
				t.Fatalf("continuation did not advance after approved action: bodies=%v; expected each distinct action exactly once", b.calls)
			}
		})
	}
}
