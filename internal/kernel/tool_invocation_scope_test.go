package kernel

import (
	"context"
	"testing"
)

func TestToolInvocationScopeRoundTrip(t *testing.T) {
	want := ToolInvocationScope{ControlTenantID: "tenant", PersonID: "person", TaskID: "task", WorkspaceID: "ws", RunID: "run", LeaseID: "lease", ExecutionScopeKey: "run:run", WorkUnitID: "wu", ExecutionLane: "main", AttachmentMode: "continuation", SkillMutationMode: SkillMutationCandidateOnly}
	ctx := WithToolInvocationScope(context.Background(), want)
	got, ok := ToolInvocationScopeFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("scope = %+v ok=%v, want %+v", got, ok, want)
	}
}
