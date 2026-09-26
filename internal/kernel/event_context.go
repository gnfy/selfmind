package kernel

import (
	"context"
	"sync/atomic"
)

type eventChannelContextKey struct{}

type streamLossContextKey struct{}

// WithEventChannel installs a per-run event channel for streaming/tool events.
// The Agent.EventChannel field remains as a legacy fallback for local TUI paths.
func WithEventChannel(ctx context.Context, ch chan string) context.Context {
	if ch == nil {
		return ctx
	}
	return context.WithValue(ctx, eventChannelContextKey{}, ch)
}

func eventChannelFromContext(ctx context.Context, fallback chan string) chan string {
	if ctx == nil {
		return fallback
	}
	if ch, ok := ctx.Value(eventChannelContextKey{}).(chan string); ok && ch != nil {
		return ch
	}
	return fallback
}

// EventChannelFromContext returns the per-run event channel installed in ctx.
// Tool packages use this to emit progress without depending on Agent internals.
func EventChannelFromContext(ctx context.Context) chan string {
	return eventChannelFromContext(ctx, nil)
}

// WithStreamLossReport lets the caller learn whether any assistant text delta
// of the run failed to reach the event channel. A consumer that assembles the
// answer from those deltas then holds an incomplete copy, and the run's own
// answer must replace it.
func WithStreamLossReport(ctx context.Context) (context.Context, func() bool) {
	lost := new(atomic.Bool)
	return context.WithValue(ctx, streamLossContextKey{}, lost), lost.Load
}

func reportStreamLoss(ctx context.Context) {
	if lost, ok := ctx.Value(streamLossContextKey{}).(*atomic.Bool); ok {
		lost.Store(true)
	}
}
