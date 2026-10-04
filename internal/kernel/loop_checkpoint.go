package kernel

import (
	"context"

	"selfmind/internal/kernel/llm"
)

// LoopCheckpoint is the latest stable boundary of one agent run. It records
// item-level model/tool history, not streaming deltas: a daemon restart can
// resume after the last completed transition without replaying a side effect
// or asking a handoff summary to reconstruct tool-call pairing.
type LoopCheckpoint struct {
	Iteration int
	Outcome   StepOutcome
	Detail    string
	Messages  []llm.Message
}

// LoopCheckpointSink persists one overwrite-only checkpoint per run.
// Implementations should be synchronous: returning means the transition is
// durable enough for crash recovery.
type LoopCheckpointSink interface {
	SaveLoopCheckpoint(ctx context.Context, checkpoint LoopCheckpoint) error
}

type loopCheckpointSinkKey struct{}
type loopResumeMessagesKey struct{}
type exactLoopReplayKey struct{}

func WithLoopCheckpointSink(ctx context.Context, sink LoopCheckpointSink) context.Context {
	if ctx == nil || sink == nil {
		return ctx
	}
	return context.WithValue(ctx, loopCheckpointSinkKey{}, sink)
}

func loopCheckpointSinkFromContext(ctx context.Context) LoopCheckpointSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(loopCheckpointSinkKey{}).(LoopCheckpointSink)
	return sink
}

// WithLoopResumeMessages installs the exact stable message ledger recovered
// from an earlier incomplete run of the same task.
func WithLoopResumeMessages(ctx context.Context, messages []llm.Message) context.Context {
	if ctx == nil || len(messages) == 0 {
		return ctx
	}
	return context.WithValue(ctx, loopResumeMessagesKey{}, cloneLoopMessages(messages))
}

func loopResumeMessagesFromContext(ctx context.Context) []llm.Message {
	if ctx == nil {
		return nil
	}
	messages, _ := ctx.Value(loopResumeMessagesKey{}).([]llm.Message)
	return cloneLoopMessages(messages)
}

// WithExactLoopReplay marks a system continuation that retries the model call
// at its saved boundary. Unlike a user continuation, it must not add the
// queue's scheduling text as a new user message to the saved ledger.
func WithExactLoopReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, exactLoopReplayKey{}, true)
}

func exactLoopReplayFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	exact, _ := ctx.Value(exactLoopReplayKey{}).(bool)
	return exact
}

// HasLoopResumeMessages reports whether the gateway installed an exact
// resumable ledger. The gateway uses it to avoid duplicating the larger
// handoff/event compatibility block in the same model request.
func HasLoopResumeMessages(ctx context.Context) bool {
	return len(loopResumeMessagesFromContext(ctx)) > 0
}

func cloneLoopMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	for i := range out {
		out[i].MultiContent = append([]llm.ContentPart(nil), messages[i].MultiContent...)
		out[i].ToolCalls = append([]llm.ToolCall(nil), messages[i].ToolCalls...)
	}
	return out
}
