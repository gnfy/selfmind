package kernel

import (
	"context"
	"sync/atomic"
)

type nestedToolBudgetKey struct{}

type nestedToolBudget struct {
	limit int64
	used  atomic.Int64
}

// WithNestedActionToolBudget installs a bounded counter for tool calls made
// from inside another tool. The returned function reports actual consumption
// so the outer agent loop can charge it to the same turn budget.
func WithNestedActionToolBudget(ctx context.Context, limit int) (context.Context, func() int) {
	if limit < 0 {
		limit = 0
	}
	budget := &nestedToolBudget{limit: int64(limit)}
	return context.WithValue(ctx, nestedToolBudgetKey{}, budget), func() int {
		return int(budget.used.Load())
	}
}

// ConsumeNestedActionTool reserves one nested action call. Direct tool tests
// and non-agent callers do not install a budget and retain their old behavior.
func ConsumeNestedActionTool(ctx context.Context) bool {
	budget, _ := ctx.Value(nestedToolBudgetKey{}).(*nestedToolBudget)
	if budget == nil {
		return true
	}
	for {
		used := budget.used.Load()
		if used >= budget.limit {
			return false
		}
		if budget.used.CompareAndSwap(used, used+1) {
			return true
		}
	}
}
