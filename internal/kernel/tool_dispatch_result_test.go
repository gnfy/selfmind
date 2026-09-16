package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

type resultFaultLedger struct {
	claimErr, outcomeErr error
	states               map[string]string
	entry                ToolLedgerEntry
}

func (l *resultFaultLedger) ClaimDispatch(_ context.Context, e ToolLedgerEntry) (ToolDispatchDecision, error) {
	l.entry = e
	if l.claimErr != nil {
		return ToolDispatchDecision{}, l.claimErr
	}
	if l.states == nil {
		l.states = map[string]string{}
	}
	if status := l.states[e.ToolCallID]; status != "" {
		return ToolDispatchDecision{Status: status}, nil
	}
	l.states[e.ToolCallID] = "started"
	return ToolDispatchDecision{Execute: true}, nil
}
func (l *resultFaultLedger) RecordOutcome(_ context.Context, run, id string, ok bool) error {
	if l.outcomeErr != nil {
		return l.outcomeErr
	}
	l.states[id] = "failed"
	if ok {
		l.states[id] = "completed"
	}
	return nil
}

type resultFactBackend struct {
	calls   int
	failure error
	output  string
}

func (*resultFactBackend) GetToolDefinitions() []map[string]interface{} { return nil }
func (*resultFactBackend) Dispatch(string, map[string]interface{}) (string, error) {
	panic("typed dispatch called the legacy path")
}
func (b *resultFactBackend) DispatchResult(string, map[string]interface{}) (ToolDispatchResult, error) {
	b.calls++
	invoked, code := true, 0
	if b.failure != nil {
		code = 7
	}
	return ToolDispatchResult{Output: b.output, Invoked: &invoked, Process: &ToolProcessResult{Started: true, ExitCode: &code}, Evidence: []RunEvidence{{ToolCallID: "call"}}}, b.failure
}

func TestTypedDispatchFailureMatrix(t *testing.T) {
	for _, phase := range []string{"claim", "execution", "outcome", "execution_and_outcome", "artifact", "success"} {
		t.Run(phase, func(t *testing.T) {
			ledger := &resultFaultLedger{}
			backend := &resultFactBackend{output: "observed-effect"}
			if phase == "claim" {
				ledger.claimErr = errors.New("claim unavailable")
			}
			if phase == "outcome" || phase == "execution_and_outcome" {
				ledger.outcomeErr = errors.New("outcome unavailable")
			}
			if phase == "execution" || phase == "execution_and_outcome" {
				backend.failure = errors.New("execution failed after partial work")
			}
			ctx := WithToolInvocationScope(context.Background(), ToolInvocationScope{RunID: "run-only", ControlTenantID: "default"})
			ctx = WithToolLedger(ctx, ledger)
			if phase == "artifact" {
				backend.output = strings.Repeat("output", 10000)
				ctx = WithToolArtifactSink(ctx, &fakeArtifactSink{fail: true})
			}
			events := make(chan string, 16)
			agent := &Agent{backend: backend}
			call := llm.ToolCall{ID: "call", Function: "terminal", Args: `{"command":"perform operation"}`}
			result := agent.executeSingleToolCall(ctx, "default", events, 0, call)
			wantCalls := 1
			if phase == "claim" {
				wantCalls = 0
			}
			if backend.calls != wantCalls || ledger.entry.RunID != "run-only" {
				t.Fatalf("calls=%d ledger=%+v", backend.calls, ledger.entry)
			}
			wantSuccess := phase == "success" || phase == "artifact"
			if result.success != wantSuccess {
				t.Fatalf("success=%v result=%s", result.success, result.msg.Content)
			}
			if ledger.outcomeErr != nil {
				for _, needle := range []string{"effect_state: unknown", "failure_phase: outcome_recording", "observed-effect"} {
					if !strings.Contains(result.msg.Content, needle) {
						t.Fatalf("missing %q: %s", needle, result.msg.Content)
					}
				}
				if ledger.states["call"] != "started" {
					t.Fatal("failed outcome erased uncertainty")
				}
			}
			if phase != "claim" {
				var completed *AgentEvent
				for _, event := range drainAgentEvents(events) {
					if event.Type == "tool.completed" {
						copy := event
						completed = &copy
					}
				}
				if completed == nil || completed.Payload["process"] == nil || completed.Payload["invoked"] != true {
					t.Fatalf("completion lost facts: %+v", completed)
				}
				// A new Agent observes the same durable claim; it cannot repeat an
				// effect just because capture, outcome storage or delivery failed.
				resumed := (&Agent{backend: backend}).executeSingleToolCall(ctx, "default", nil, 0, call)
				if resumed.success || backend.calls != 1 {
					t.Fatalf("duplicate effect: calls=%d", backend.calls)
				}
			}
		})
	}
}

func TestTypedDispatchRejectsConflictingRunAuthority(t *testing.T) {
	backend := &resultFactBackend{}
	ctx := WithToolInvocationScope(context.Background(), ToolInvocationScope{RunID: "current"})
	ctx = WithTaskRuntimeContext(ctx, TaskRuntimeContext{RunID: "other"})
	ctx = WithToolLedger(ctx, &resultFaultLedger{})
	result := (&Agent{backend: backend}).executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{ID: "call", Function: "terminal", Args: `{}`})
	if result.success || backend.calls != 0 || !strings.Contains(result.msg.Content, "does not match") {
		t.Fatalf("conflicting Run accepted: %+v", result)
	}
}

func TestLegacyBackendDoesNotInventProcessFacts(t *testing.T) {
	result, err := DispatchToolResult(successfulToolBackend{}, "legacy", nil)
	if err != nil || result.Output != "ok" || result.Invoked != nil || result.Process != nil || len(result.Evidence) != 0 {
		t.Fatalf("invented facts: %+v err=%v", result, err)
	}
}
