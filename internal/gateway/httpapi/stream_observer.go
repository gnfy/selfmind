package httpapi

import (
	"context"

	"selfmind/internal/kernel/llm"
)

type streamObserverKey struct{}

type StreamObserver func(llm.StreamEvent)

func WithStreamObserver(ctx context.Context, observer StreamObserver) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	return context.WithValue(ctx, streamObserverKey{}, observer)
}

func streamObserverFromContext(ctx context.Context) StreamObserver {
	return StreamObserverFromContext(ctx)
}

// StreamObserverFromContext returns the stream observer installed by
// WithStreamObserver, or nil. It is exported so a daemon-backed MessageProcessor
// (running out-of-process from the agent) can pump remote SSE run events into
// the same observer the in-process path uses, keeping TUI rendering identical
// whether the agent runs locally or in a shared gateway daemon.
func StreamObserverFromContext(ctx context.Context) StreamObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(streamObserverKey{}).(StreamObserver)
	return observer
}

// The coordinator owns these identities; raw kernel events need not contain
// them. Observers receive the same ownership as the durable event projection.
func withRunStreamIdentity(observer StreamObserver, taskID, runID string) StreamObserver {
	if observer == nil {
		return nil
	}
	return func(event llm.StreamEvent) {
		if runID != "" {
			event.RunID = runID
		}
		if taskID != "" {
			event.TaskID = taskID
		}
		observer(event)
	}
}
