package tools

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
)

type delegateContextKey struct{}

func TestDelegateToolForwardsAuthenticatedRunContext(t *testing.T) {
	tool := NewDelegateTool()
	tool.RegisterDelegateFn(func(ctx context.Context, goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
		if got := ctx.Value(delegateContextKey{}); got != "run-scope" {
			t.Fatalf("delegate context value = %v", got)
		}
		if goal != "inspect" || contextStr != "supporting" {
			t.Fatalf("goal/context = %q/%q", goal, contextStr)
		}
		return "done", llm.UsageStats{}, nil
	})
	ctx := context.WithValue(context.Background(), delegateContextKey{}, "run-scope")
	out, err := tool.ExecuteContext(ctx, map[string]interface{}{"goal": "inspect", "context": "supporting"})
	if err != nil || out != "done" {
		t.Fatalf("ExecuteContext = %q, %v", out, err)
	}
}
