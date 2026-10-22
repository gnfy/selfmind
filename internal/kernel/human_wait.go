package kernel

import (
	"context"
	"fmt"
	"sync/atomic"

	"selfmind/internal/kernel/llm"
)

// Human wait boundaries are in-process facts installed by the loop, never
// supplied by tool arguments or model output. The pending call has not produced
// an effect; completed call/results before it stay in the checkpoint ledger.
type humanWaitBoundary struct {
	checkpoint LoopCheckpoint
	call       llm.ToolCall
	entered    bool
}
type humanWaitBoundaryKey struct{}
type humanWaitParkedKey struct{}

func withHumanWaitBatch(ctx context.Context, checkpoint LoopCheckpoint) context.Context {
	return context.WithValue(ctx, humanWaitBoundaryKey{}, &humanWaitBoundary{checkpoint: checkpoint})
}
func withHumanWaitCall(ctx context.Context, call llm.ToolCall) context.Context {
	batch, _ := ctx.Value(humanWaitBoundaryKey{}).(*humanWaitBoundary)
	if batch == nil {
		return ctx
	}
	boundary := &humanWaitBoundary{checkpoint: batch.checkpoint, call: call}
	return context.WithValue(ctx, humanWaitBoundaryKey{}, boundary)
}

// MarkToolInvocationStarted is called at the registry's actual tool-body
// boundary. An approval before this point may suspend; an opaque tool that has
// already started (including nested delegation) must keep its cancellable worker.
func MarkToolInvocationStarted(ctx context.Context) {
	if boundary, _ := ctx.Value(humanWaitBoundaryKey{}).(*humanWaitBoundary); boundary != nil && boundary.call.ID != "" {
		boundary.entered = true
	}
}

func HumanWaitToolCallID(ctx context.Context) string {
	if boundary, _ := ctx.Value(humanWaitBoundaryKey{}).(*humanWaitBoundary); boundary != nil {
		return boundary.call.ID
	}
	return ""
}

// SaveHumanWaitCheckpoint persists the exact pending dispatch boundary. Only a
// direct approval before invocation, or the effect-free clarify tool, is safe.
func SaveHumanWaitCheckpoint(ctx context.Context, kind, ref, toolName string) error {
	return saveToolWaitCheckpoint(ctx, kind, ref, toolName, "await_human", "human_wait:")
}

// SaveExecutionWaitCheckpoint uses the same exact uninvoked batch boundary as
// a human wait. A lane grants no permission; it only delays dispatch.
func SaveExecutionWaitCheckpoint(ctx context.Context, runID, toolName string, host ...bool) error {
	kind := "execution_lane"
	if len(host) > 0 && host[0] {
		kind = "execution_host_lane"
	}
	return saveToolWaitCheckpoint(ctx, kind, runID, toolName, "await_execution", "execution_wait:")
}

func saveToolWaitCheckpoint(ctx context.Context, kind, ref, toolName, outcome, detailPrefix string) error {
	boundary, _ := ctx.Value(humanWaitBoundaryKey{}).(*humanWaitBoundary)
	if boundary == nil || boundary.call.ID == "" || boundary.call.Function != toolName || DelegationNamespace(ctx) != "" {
		return fmt.Errorf("human wait has no replayable top-level tool boundary")
	}
	if boundary.entered && !(kind == "clarification" && toolName == "clarify") {
		return fmt.Errorf("tool invocation already started; its partial effects cannot be replayed")
	}
	sink := loopCheckpointSinkFromContext(ctx)
	if sink == nil {
		return fmt.Errorf("human wait checkpoint sink is unavailable")
	}
	checkpoint := boundary.checkpoint
	checkpoint.Outcome = StepOutcome(outcome)
	checkpoint.Detail = detailPrefix + kind + ":" + ref
	checkpoint.WaitKind, checkpoint.WaitRef, checkpoint.WaitToolCallID = kind, ref, boundary.call.ID
	checkpoint.Messages = cloneLoopMessages(checkpoint.Messages)
	return sink.SaveLoopCheckpoint(context.WithoutCancel(ctx), checkpoint)
}

func MarkHumanWaitParked(ctx context.Context) {
	if parked, _ := ctx.Value(humanWaitParkedKey{}).(*atomic.Bool); parked != nil {
		parked.Store(true)
	}
}

// MarkExecutionWaitParked preserves the pending batch across the final pause.
func MarkExecutionWaitParked(ctx context.Context) { MarkHumanWaitParked(ctx) }

// WithExecutionWaitResume dispatches only the exact unpaired saved tail. It
// confers no authority; the new call still crosses every current tool policy.
func WithExecutionWaitResume(ctx context.Context, toolCallID string) context.Context {
	return WithHumanWaitResume(ctx, HumanWaitResume{ToolCallID: toolCallID})
}

func humanWaitParked(ctx context.Context) bool {
	parked, _ := ctx.Value(humanWaitParkedKey{}).(*atomic.Bool)
	return parked != nil && parked.Load()
}

// HumanWaitResume is runtime-owned resolution of a typed checkpoint. A nil
// result dispatches the uninvoked approved call; clarification supplies its
// paired result. Denial answers every unexecuted call without dispatch.
type HumanWaitResume struct {
	ToolCallID    string
	Result        *llm.Message
	Denied        bool
	RecheckReason string
}
type humanWaitResumeKey struct{}

func WithHumanWaitResume(ctx context.Context, resume HumanWaitResume) context.Context {
	return WithExactLoopReplay(context.WithValue(ctx, humanWaitResumeKey{}, resume))
}
func humanWaitResumeFromContext(ctx context.Context) (HumanWaitResume, bool) {
	resume, ok := ctx.Value(humanWaitResumeKey{}).(HumanWaitResume)
	return resume, ok
}

// pendingHumanWaitCalls permits only the checkpoint's exact unpaired tail.
// Completed call/result pairs stay byte-identical; none are re-dispatched.
func pendingHumanWaitCalls(messages []llm.Message, callID string) ([]llm.ToolCall, error) {
	lastAssistant := -1
	for i := range messages {
		if len(messages[i].ToolCalls) > 0 {
			lastAssistant = i
		}
	}
	if lastAssistant < 0 {
		return nil, fmt.Errorf("human wait checkpoint has no pending tool batch")
	}
	answered := make(map[string]bool)
	for _, m := range messages[lastAssistant+1:] {
		if m.Role != "tool" || m.ToolCallID == "" || answered[m.ToolCallID] {
			return nil, fmt.Errorf("human wait checkpoint has an invalid result tail")
		}
		answered[m.ToolCallID] = true
	}
	var pending []llm.ToolCall
	known := make(map[string]bool)
	for _, call := range messages[lastAssistant].ToolCalls {
		if call.ID == "" || known[call.ID] {
			return nil, fmt.Errorf("human wait checkpoint has invalid call identifiers")
		}
		known[call.ID] = true
	}
	for id := range answered {
		if !known[id] {
			return nil, fmt.Errorf("human wait checkpoint has an unpaired result")
		}
	}
	for _, call := range messages[lastAssistant].ToolCalls {
		if !answered[call.ID] {
			pending = append(pending, call)
		}
	}
	if len(pending) == 0 || pending[0].ID != callID {
		return nil, fmt.Errorf("human wait checkpoint does not match the suspended call")
	}
	return pending, nil
}

type deniedHumanWaitError struct{}

func (deniedHumanWaitError) Error() string {
	return "operation rejected: the person rejected this approval; this batch was not dispatched."
}
func (deniedHumanWaitError) ToolErrorCode() string      { return "approval_rejected" }
func (deniedHumanWaitError) ToolErrorCategory() string  { return "rejected" }
func (e deniedHumanWaitError) ModelSafeMessage() string { return e.Error() }
func (deniedHumanWaitError) ToolRecoveryHint() string {
	return "This is a user decision. Do not retry this operation or a variant; report what was not done and continue only with different work, or finish."
}
func (deniedHumanWaitError) ToolFailurePhase() string   { return "authorization" }
func (deniedHumanWaitError) ToolRetryability() string   { return "user_decision" }
func (deniedHumanWaitError) ToolEffectState() string    { return "not_dispatched" }
func (deniedHumanWaitError) ToolStateChanged() bool     { return false }
func (deniedHumanWaitError) ToolAlternatives() []string { return nil }

func (a *Agent) resumeHumanWaitCalls(ctx context.Context, tenantID string, eventCh chan string, messages []llm.Message, resume HumanWaitResume) ([]toolExecutionResult, error) {
	calls, err := pendingHumanWaitCalls(messages, resume.ToolCallID)
	if err != nil {
		return nil, err
	}
	if resume.Denied {
		results := make([]toolExecutionResult, len(calls))
		for i, call := range calls {
			results[i] = a.toolDispatchRefused(eventCh, i, call, call.Function+"\x00"+call.Args, deniedHumanWaitError{})
		}
		return results, nil
	}
	var results []toolExecutionResult
	if resume.Result != nil {
		if resume.Result.Role != "tool" || resume.Result.ToolCallID != calls[0].ID {
			return nil, fmt.Errorf("human answer has no exact paired result")
		}
		results = append(results, toolExecutionResult{msg: *resume.Result, toolName: calls[0].Function, success: true, rawResult: resume.Result.Content, step: "Resumed human answer", effectState: "not_dispatched"})
		messages = append(cloneLoopMessages(messages), *resume.Result)
		calls = calls[1:]
	}
	if resume.RecheckReason != "" {
		for i, call := range calls {
			results = append(results, a.toolDispatchRefused(eventCh, i, call, call.Function+"\x00"+call.Args, resumedWorkspaceChangedError{reason: resume.RecheckReason}))
		}
		return results, nil
	}
	if len(calls) > 0 {
		toolCtx := withHumanWaitBatch(ctx, LoopCheckpoint{Outcome: StepExecuteTools, Messages: cloneLoopMessages(messages)})
		results = append(results, a.executeToolCalls(toolCtx, tenantID, eventCh, calls)...)
	}
	return results, nil
}

// Revalidation is a non-dispatch, not rejection by the person or a failed
// execution. Main receives the saved evidence and decides what to observe next.
type resumedWorkspaceChangedError struct{ reason string }

func (e resumedWorkspaceChangedError) Error() string {
	return "Saved pending call was not dispatched: " + e.reason
}
func (resumedWorkspaceChangedError) ToolErrorCode() string      { return "resume_revalidation_required" }
func (resumedWorkspaceChangedError) ToolErrorCategory() string  { return "state_changed" }
func (e resumedWorkspaceChangedError) ModelSafeMessage() string { return e.Error() }
func (resumedWorkspaceChangedError) ToolRecoveryHint() string {
	return "Retain the person's answer and original goal. Prior tool results are historical evidence. Observe the affected files or targets before deciding whether this action is still needed, then submit a fresh call through current policy. Do not repeat completed effects."
}
func (resumedWorkspaceChangedError) ToolFailurePhase() string   { return "preparation" }
func (resumedWorkspaceChangedError) ToolRetryability() string   { return "after_observation" }
func (resumedWorkspaceChangedError) ToolEffectState() string    { return "not_dispatched" }
func (resumedWorkspaceChangedError) ToolStateChanged() bool     { return false }
func (resumedWorkspaceChangedError) ToolAlternatives() []string { return nil }
