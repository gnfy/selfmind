package kernel

import (
	"context"
	"strings"
)

// Steering lets a client inject additional user input INTO a running turn
// (codex/Claude-style mid-turn guidance) instead of rejecting it as "busy" or
// dropping it. The UI owns a channel and pushes the user's follow-up text onto
// it while the agent loop is running; the loop drains it at each iteration
// boundary and appends it as a user message before the next model call, so the
// model adjusts course in the same turn.

type steeringCtxKey struct{}

// WithSteering attaches a steering channel to ctx. A nil channel is a no-op.
func WithSteering(ctx context.Context, ch <-chan string) context.Context {
	if ch == nil {
		return ctx
	}
	return context.WithValue(ctx, steeringCtxKey{}, ch)
}

func steeringFromContext(ctx context.Context) <-chan string {
	if ctx == nil {
		return nil
	}
	if ch, ok := ctx.Value(steeringCtxKey{}).(<-chan string); ok {
		return ch
	}
	return nil
}

// drainSteering non-blockingly collects all pending steering messages.
func drainSteering(ch <-chan string) []string {
	if ch == nil {
		return nil
	}
	var out []string
	for {
		select {
		case m := <-ch:
			if s := strings.TrimSpace(m); s != "" {
				out = append(out, s)
			}
		default:
			return out
		}
	}
}
