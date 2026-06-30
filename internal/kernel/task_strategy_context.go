package kernel

import "context"

type taskStrategyContextKey struct{}

// WithTaskStrategy pins a per-turn strategy chosen by the gateway before it
// adds system/task metadata to the user prompt.
func WithTaskStrategy(ctx context.Context, strategy TaskStrategy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, taskStrategyContextKey{}, strategy.normalized())
}

func taskStrategyFromContext(ctx context.Context) (TaskStrategy, bool) {
	return TaskStrategyFromContext(ctx)
}

// TaskStrategyFromContext returns the per-turn strategy pinned by the gateway,
// if any. Exported so the gateway scheduler can read the strategy to decide
// write-vs-read workspace serialization (see Gateway.workspaceSerialKey).
func TaskStrategyFromContext(ctx context.Context) (TaskStrategy, bool) {
	if ctx == nil {
		return TaskStrategy{}, false
	}
	strategy, ok := ctx.Value(taskStrategyContextKey{}).(TaskStrategy)
	if !ok {
		return TaskStrategy{}, false
	}
	return strategy.normalized(), true
}
