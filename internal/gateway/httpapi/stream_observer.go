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
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(streamObserverKey{}).(StreamObserver)
	return observer
}
