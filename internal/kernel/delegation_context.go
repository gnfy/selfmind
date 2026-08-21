package kernel

import (
	"context"
	"time"
)

// ForkDelegationContext preserves the parent's lifetime and execution
// authority without inheriting loop-local strategy, steering, checkpoint, or
// deferred-tool activation state. Each sub-agent therefore gets a fresh loop
// while its tools remain scoped to the parent run and workspace.
func ForkDelegationContext(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	return delegationContext{parent: parent}
}

type delegationContext struct {
	parent context.Context
}

func (c delegationContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c delegationContext) Done() <-chan struct{}       { return c.parent.Done() }
func (c delegationContext) Err() error                  { return c.parent.Err() }

func (c delegationContext) Value(key any) any {
	switch key.(type) {
	case workspaceContextKey,
		taskRuntimeContextKey,
		runtimeContextBundleKey,
		toolInvocationScopeContextKey,
		eventChannelContextKey,
		toolLedgerKey,
		toolArtifactSinkKey,
		skillRuntimeContextKey:
		return c.parent.Value(key)
	default:
		return nil
	}
}
