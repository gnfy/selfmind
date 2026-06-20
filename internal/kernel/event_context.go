package kernel

import "context"

type eventChannelContextKey struct{}

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
