package kernel

import (
	"context"
	"testing"
)

func TestForkDelegationContextPreservesAuthorityButNotLoopState(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	parent = WithWorkspaceContext(parent, WorkspaceContext{ID: "ws_1", Root: t.TempDir()})
	parent = WithTaskRuntimeContext(parent, TaskRuntimeContext{TaskID: "task_1", RunID: "run_1"})
	parent = WithTaskStrategy(parent, DefaultTaskStrategy())
	parent = withToolActivationState(parent)
	activateDeferredTools(parent, []string{"deferred_probe"})

	child := ForkDelegationContext(parent)
	if workspace, ok := WorkspaceContextFromContext(child); !ok || workspace.ID != "ws_1" {
		t.Fatalf("workspace authority was not propagated: %+v, %v", workspace, ok)
	}
	if runtime, ok := TaskRuntimeContextFromContext(child); !ok || runtime.RunID != "run_1" {
		t.Fatalf("run context was not propagated: %+v, %v", runtime, ok)
	}
	if _, ok := TaskStrategyFromContext(child); ok {
		t.Fatal("parent strategy leaked into the delegated loop")
	}
	if _, ok := toolActivationStateFromContext(child); ok {
		t.Fatal("parent deferred-tool activation state leaked into the delegated loop")
	}

	cancel()
	<-child.Done()
	if child.Err() != context.Canceled {
		t.Fatalf("child cancellation = %v, want context.Canceled", child.Err())
	}
}
