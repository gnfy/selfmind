package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type mediumResultBackend struct{ budgetBackend }

func (*mediumResultBackend) Dispatch(_ string, args map[string]interface{}) (string, error) {
	return fmt.Sprint(args["path"]) + strings.Repeat("x", 16000), nil
}

type mediumResultProvider struct {
	multiToolProvider
	maxBytes int
	// seen records each tool result the FIRST time a request carried it, keyed
	// by tool-call id, so a later request that changed one is detectable.
	seen     map[string]string
	rewrites []string
}

func (p *mediumResultProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	if n := liveToolResultBytes(req.Messages); n > p.maxBytes {
		p.maxBytes = n
	}
	if p.seen == nil {
		p.seen = map[string]string{}
	}
	for _, msg := range req.Messages {
		if msg.Role != "tool" {
			continue
		}
		if before, ok := p.seen[msg.ToolCallID]; ok {
			if before != msg.Content {
				p.rewrites = append(p.rewrites, msg.ToolCallID)
			}
			continue
		}
		p.seen[msg.ToolCallID] = msg.Content
	}
	return p.multiToolProvider.StreamChat(ctx, req)
}

// TestRunNeverRewritesASentToolResult is the prompt-cache contract. A tool
// result's bytes are decided when it is packaged; changing them afterwards
// moves the request prefix, and the provider's cache breaks from the first
// changed message onward. Five results that each fit the per-result cap but
// together exceed the old 32 KiB rolling window must therefore reach the
// provider whole and stay byte-identical for the rest of the turn.
//
// Measured on 2026-09-18, live tool results averaged 71 KiB per run against
// that 32 KiB window, so it fired on nearly every turn: 166 of 192 cold
// provider calls immediately followed one of its rewrites, and cold calls
// carried 7.7M of the day's 9.2M uncached input tokens. The window bought back
// tens of kilobytes of cached tokens by making hundreds of kilobytes uncached.
//
// The spool_failure branch keeps the original incident's guard: evidence that
// could not be made addressable is never shortened.
func TestRunNeverRewritesASentToolResult(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("spool_failure=", fail), func(t *testing.T) {
			provider := &mediumResultProvider{multiToolProvider: multiToolProvider{toolTurns: 5}}
			sink := &fakeArtifactSink{fail: fail}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &mediumResultBackend{}, provider, "helpful", 10, 1, nil)
			ctx := WithToolArtifactSink(context.Background(), sink)
			if _, _, err := agent.RunConversation(ctx, "test", "cli", "inspect five files"); err != nil {
				t.Fatal(err)
			}
			if len(provider.rewrites) != 0 {
				t.Fatalf("results were rewritten after being sent, breaking the prefix: %v", provider.rewrites)
			}
			// Whole, not shrunk: five 16000-byte results are the ordinary
			// working size and must simply be carried.
			if provider.maxBytes < 5*16000 {
				t.Fatalf("a result was shortened before the reclaim ceiling: max=%d", provider.maxBytes)
			}
			if provider.maxBytes > toolResultReclaimCeilingBytes {
				t.Fatalf("the reclaim ceiling was not enforced: max=%d", provider.maxBytes)
			}
		})
	}
}

type closurePreconditionError struct{}

func (closurePreconditionError) Error() string             { return "unresolved plan" }
func (closurePreconditionError) ToolErrorCode() string     { return "completion_precondition" }
func (closurePreconditionError) ToolErrorCategory() string { return "stale_precondition" }
func (closurePreconditionError) ModelSafeMessage() string {
	return "Resolve the plan before finishing."
}
func (closurePreconditionError) ToolRecoveryHint() string { return "Update the plan." }

type repairClosureBackend struct {
	budgetClosureBackend
	attempts int
}

func (b *repairClosureBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name == "finish_run" {
		b.attempts++
		if b.attempts == 1 {
			return "", closurePreconditionError{}
		}
	}
	return b.budgetClosureBackend.Dispatch(name, args)
}

type repairClosureProvider struct {
	mockLLMProvider
	requests     int
	changed      bool
	retryVisible bool
}

func (p *repairClosureProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests++
	ch := make(chan llm.StreamEvent, 1)
	switch p.requests {
	case 1, 3:
		if p.requests == 3 {
			p.retryVisible = requestHasTool(req, "finish_run")
		}
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: fmt.Sprint("finish-", p.requests), Function: "finish_run", Args: `{"status":"done","summary":"done"}`}}}
	case 2:
		if p.changed {
			ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "correct-plan", Function: "update_plan", Args: `{"plan":[{"step":"inspect","status":"completed"}]}`}}}
		} else {
			ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "unchanged-finish", Function: "finish_run", Args: `{"status":"done","summary":"done"}`}}}
		}
	default:
		ch <- llm.StreamEvent{Content: "Finished from the recorded evidence."}
	}
	close(ch)
	return ch, nil
}

func TestFailedFinishCanRetryOnlyAfterPlanCorrection(t *testing.T) {
	for _, changed := range []bool{true, false} {
		t.Run(fmt.Sprint("changed=", changed), func(t *testing.T) {
			backend := &repairClosureBackend{}
			provider := &repairClosureProvider{changed: changed}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "helpful", 8, 1, nil)
			if _, _, err := agent.RunConversation(context.Background(), "test", "cli", "complete the work"); err != nil {
				t.Fatal(err)
			}
			want := 1
			if changed {
				want = 2
			}
			if backend.attempts != want || provider.retryVisible != changed {
				t.Fatalf("finish attempts=%d want=%d retry visible=%v", backend.attempts, want, provider.retryVisible)
			}
		})
	}
}

func TestActionBatchPreservesCompletionReserve(t *testing.T) {
	strategy := DefaultTaskStrategy()
	strategy.MaxActionTools, strategy.ActionToolBudgetLimit, strategy.CompletionReserve = 12, 12, 2
	calls := []llm.ToolCall{{Function: "write_file"}, {Function: "write_file"}, {Function: "verify"}, {Function: "verify"}, {Function: "verify"}, {Function: "finish_run"}}
	got, dropped := filterToolCallsByStrategyAndBudget(calls, strategy, 9)
	if dropped != 2 || len(got) != 4 || got[0].Function != "write_file" || got[1].Function != "verify" || got[2].Function != "verify" || got[3].Function != "finish_run" {
		t.Fatalf("batch consumed verification reserve: kept=%v dropped=%d", got, dropped)
	}
}

func TestActionToolsDisabledDropsReserveWithoutMutatingOriginal(t *testing.T) {
	strategy := DefaultTaskStrategy()
	strategy.AllowedTools = map[string]bool{"verify": true, "read_file": true}
	closed := strategy.WithActionToolsDisabled()
	if closed.AllowsTool("verify") || closed.AllowsTool("read_file") || !closed.AllowsTool("finish_run") {
		t.Fatalf("closed strategy retains action tools: %+v", closed)
	}
	if len(strategy.AllowedTools) != 2 || !strategy.AllowsTool("verify") {
		t.Fatal("closing the iteration mutated its parent strategy")
	}
}

func TestFinishCorrectionNeverUnlocksUnchangedOrRepeatedFailures(t *testing.T) {
	for _, code := range []string{"completion_precondition", "approval_rejected", "unknown"} {
		counts := map[string]int{"finish_run": 1}
		var correction finishCorrection
		correction.observe([]toolExecutionResult{{toolName: "finish_run", errorCode: code}}, counts)
		correction.observe([]toolExecutionResult{{toolName: "update_plan", success: true, rawResult: `{"changed":false}`}}, counts)
		if counts["finish_run"] != 1 {
			t.Fatal("no-op plan unlocked finish")
		}
		correction.observe([]toolExecutionResult{{toolName: "verify", success: true}}, counts)
		want := 1
		if code == "completion_precondition" {
			want = 0
		}
		if counts["finish_run"] != want {
			t.Fatalf("%s unlocked incorrectly: %v", code, counts)
		}
		counts["finish_run"] = 1
		correction.observe([]toolExecutionResult{{toolName: "finish_run", errorCode: code}, {toolName: "update_plan", success: true, rawResult: `{"changed":true}`}}, counts)
		if counts["finish_run"] != 1 {
			t.Fatal("second correction exceeded bounded finish attempts")
		}
	}
}
