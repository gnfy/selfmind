package kernel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type humanCheckpointRecorder struct {
	saved   []LoopCheckpoint
	failure error
}

func (s *humanCheckpointRecorder) SaveLoopCheckpoint(_ context.Context, cp LoopCheckpoint) error {
	if s.failure != nil {
		return s.failure
	}
	s.saved = append(s.saved, cp)
	return nil
}
func TestHumanWaitCheckpointRefusesStartedOrNestedEffects(t *testing.T) {
	for _, name := range []string{"write_file", "opaque_publish"} {
		t.Run(name, func(t *testing.T) {
			sink := &humanCheckpointRecorder{}
			ctx := WithLoopCheckpointSink(context.Background(), sink)
			ctx = withHumanWaitBatch(ctx, LoopCheckpoint{Messages: []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "pending", Function: name, Args: `{}`}}}}})
			ctx = withHumanWaitCall(ctx, llm.ToolCall{ID: "pending", Function: name, Args: `{}`})
			MarkToolInvocationStarted(ctx)
			if err := SaveHumanWaitCheckpoint(ctx, "approval", "approval-id", name); err == nil || len(sink.saved) != 0 {
				t.Fatal("partly invoked action became replayable")
			}
		})
	}
	sink := &humanCheckpointRecorder{}
	ctx := WithLoopCheckpointSink(context.Background(), sink)
	ctx = withHumanWaitBatch(ctx, LoopCheckpoint{})
	ctx = withHumanWaitCall(ctx, llm.ToolCall{ID: "nested-call", Function: "clarify"})
	ctx = WithDelegationNamespace(ctx, "child")
	if err := SaveHumanWaitCheckpoint(ctx, "clarification", "question-id", "clarify"); err == nil || len(sink.saved) != 0 {
		t.Fatal("nested delegation checkpointed as top-level")
	}
}
func TestHumanWaitCheckpointRetainsPairingAndFailureMeaning(t *testing.T) {
	sink := &humanCheckpointRecorder{}
	messages := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read", Function: "read_file"}, {ID: "pending", Function: "write_file"}}}, {Role: "tool", ToolCallID: "read", Content: "original evidence"}}
	ctx := WithLoopCheckpointSink(context.Background(), sink)
	ctx = withHumanWaitBatch(ctx, LoopCheckpoint{Messages: messages})
	ctx = withHumanWaitCall(ctx, llm.ToolCall{ID: "pending", Function: "write_file"})
	if err := SaveHumanWaitCheckpoint(ctx, "approval", "approval-id", "write_file"); err != nil {
		t.Fatal(err)
	}
	cp := sink.saved[0]
	if cp.Outcome != "await_human" || cp.WaitToolCallID != "pending" || cp.Messages[1].ToolCallID != "read" || cp.Messages[1].Content != "original evidence" {
		t.Fatalf("lost suspended boundary: %+v", cp)
	}
	if calls, err := pendingHumanWaitCalls(cp.Messages, "pending"); err != nil || len(calls) != 1 || calls[0].ID != "pending" {
		t.Fatalf("read would replay: %+v %v", calls, err)
	}
	sink.failure = errors.New("injected persistence failure")
	if err := SaveHumanWaitCheckpoint(ctx, "approval", "approval-id", "write_file"); err == nil || humanWaitParked(ctx) {
		t.Fatal("persistence failure became parked success")
	}
}
func TestHumanWaitResumeRejectsUnpairedOrDifferentCall(t *testing.T) {
	valid := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "pending", Function: "write_file"}}}}
	for _, messages := range [][]llm.Message{valid, append(cloneLoopMessages(valid), llm.Message{Role: "tool", ToolCallID: "different", Content: "unrelated"}), append(cloneLoopMessages(valid), llm.Message{Role: "assistant", Content: "not a dispatch checkpoint"})} {
		id := "pending"
		if len(messages) == 1 {
			id = "different"
		}
		if _, err := pendingHumanWaitCalls(messages, id); err == nil {
			t.Fatalf("invalid ledger accepted: %+v", messages)
		}
	}
}

func TestHumanWaitResumedFinishLeavesOnlyFinalAnswer(t *testing.T) {
	provider := &postFinishProvider{requests: 1}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &budgetClosureBackend{}, provider, "helpful", 4, 1, nil)
	messages := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{
		{ID: "pending-read", Function: "read_file", Args: `{"path":"target.go"}`},
		{ID: "pending-finish", Function: "finish_run", Args: `{"status":"done","summary":"verified"}`},
	}}}
	ctx := WithLoopResumeMessages(context.Background(), messages)
	ctx = WithHumanWaitResume(ctx, HumanWaitResume{ToolCallID: "pending-read"})
	answer, _, err := agent.RunConversation(ctx, "test", "cli", "report verified result")
	if err != nil {
		t.Fatal(err)
	}
	if provider.toolsAfterFinish != 0 || answer != "Verified result." {
		t.Fatalf("resumed finish left tools=%d answer=%q", provider.toolsAfterFinish, answer)
	}
}

func TestHumanWaitDenialKeepsTypedDecisionMeaning(t *testing.T) {
	envelope := packageToolError("write_file", deniedHumanWaitError{})
	if envelope.ErrorCategory != "rejected" || envelope.ErrorCode != "approval_rejected" || envelope.EffectState != "not_dispatched" || envelope.Retryability != "user_decision" {
		t.Fatalf("denial became an execution failure: %+v", envelope)
	}
}

func TestHumanWaitRevalidationPreservesAnswerAndRefusesSavedTail(t *testing.T) {
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &budgetClosureBackend{}, &mockLLMProvider{}, "helpful", 4, 1, nil)
	for _, clarification := range []bool{false, true} {
		calls := []llm.ToolCall{{ID: "write", Function: "write_file", Args: `{}`}, {ID: "finish", Function: "finish_run", Args: `{}`}}
		resume := HumanWaitResume{ToolCallID: "write", RecheckReason: "overlapping workspace activity"}
		if clarification {
			calls = append([]llm.ToolCall{{ID: "ask", Function: "clarify"}}, calls...)
			resume.ToolCallID = "ask"
			resume.Result = &llm.Message{Role: "tool", ToolCallID: "ask", Content: "the person's exact answer"}
		}
		messages := []llm.Message{{Role: "assistant", ToolCalls: calls}}
		results, err := agent.resumeHumanWaitCalls(context.Background(), "test", nil, messages, resume)
		if err != nil || len(results) != len(calls) {
			t.Fatalf("results=%+v err=%v", results, err)
		}
		for i, result := range results {
			if clarification && i == 0 {
				if !result.success || result.msg.Content != resume.Result.Content {
					t.Fatal("human answer was lost")
				}
				continue
			}
			if result.success || result.effectState != "not_dispatched" || !strings.Contains(result.msg.Content, "resume_revalidation_required") {
				t.Fatalf("saved tail ran or lost refusal: %+v", result)
			}
		}
	}
}

type humanWaitParallelBackend struct{ budgetClosureBackend }

func (*humanWaitParallelBackend) SupportsParallelTool(string) bool { return true }
func (*humanWaitParallelBackend) Dispatch(_ string, args map[string]interface{}) (string, error) {
	ctx, _ := args["_context"].(context.Context)
	MarkToolInvocationStarted(ctx)
	return "read evidence", nil
}

func TestHumanWaitParallelReadsDoNotMutateTheBatchBoundary(t *testing.T) {
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &humanWaitParallelBackend{}, &mockLLMProvider{}, "helpful", 4, 1, nil)
	ctx := withHumanWaitBatch(context.Background(), LoopCheckpoint{})
	calls := make([]llm.ToolCall, 12)
	for i := range calls {
		calls[i] = llm.ToolCall{ID: fmt.Sprintf("read-%d", i), Function: "read_file", Args: `{}`}
	}
	results := agent.executeToolCalls(ctx, "test", nil, calls)
	for _, result := range results {
		if !result.success {
			t.Fatalf("parallel read failed: %+v", result)
		}
	}
	boundary := ctx.Value(humanWaitBoundaryKey{}).(*humanWaitBoundary)
	if boundary.entered {
		t.Fatal("parallel reads mutated the shared batch placeholder")
	}
}

func TestExecutionWaitCheckpointUsesExactUninvokedBoundary(t *testing.T) {
	for _, boundary := range []string{"exact", "entered", "nested", "missing_sink", "different_tool"} {
		t.Run(boundary, func(t *testing.T) {
			sink := &humanCheckpointRecorder{}
			ctx := context.Background()
			if boundary != "missing_sink" {
				ctx = WithLoopCheckpointSink(ctx, sink)
			}
			messages := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read", Function: "read_file"}, {ID: "pending", Function: "write_file"}}}, {Role: "tool", ToolCallID: "read", Content: "saved evidence"}}
			ctx = withHumanWaitBatch(ctx, LoopCheckpoint{Iteration: 7, Messages: messages})
			ctx = withHumanWaitCall(ctx, llm.ToolCall{ID: "pending", Function: "write_file"})
			if boundary == "entered" {
				MarkToolInvocationStarted(ctx)
			}
			if boundary == "nested" {
				ctx = WithDelegationNamespace(ctx, "nested-worker")
			}
			name := "write_file"
			if boundary == "different_tool" {
				name = "terminal"
			}
			err := SaveExecutionWaitCheckpoint(ctx, "original-run", name)
			if boundary != "exact" {
				if err == nil || len(sink.saved) != 0 {
					t.Fatalf("unsafe boundary saved: %+v %v", sink.saved, err)
				}
				return
			}
			if err != nil || len(sink.saved) != 1 {
				t.Fatalf("exact boundary refused: %v", err)
			}
			cp := sink.saved[0]
			if cp.Outcome != "await_execution" || cp.Iteration != 7 || cp.WaitKind != "execution_lane" || cp.WaitRef != "original-run" || cp.WaitToolCallID != "pending" || cp.Messages[1].Content != "saved evidence" {
				t.Fatalf("lost exact execution wait: %+v", cp)
			}
			sink.failure = fmt.Errorf("injected write failure")
			if err := SaveExecutionWaitCheckpoint(ctx, "original-run", name); err == nil || humanWaitParked(ctx) {
				t.Fatal("save failure became parked")
			}
		})
	}
}
