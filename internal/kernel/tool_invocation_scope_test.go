package kernel

import (
	"context"
	"testing"
)

func TestToolInvocationScopeRoundTrip(t *testing.T) {
	want := ToolInvocationScope{ControlTenantID: "tenant", PersonID: "person", WorkspaceID: "ws", RunID: "run", LeaseID: "lease", ExecutionScopeKey: "run:run"}
	ctx := WithToolInvocationScope(context.Background(), want)
	got, ok := ToolInvocationScopeFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("scope = %+v ok=%v, want %+v", got, ok, want)
	}
}
