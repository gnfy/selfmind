package kernel

import (
	"context"
	"sync"
)

// RunExecutionState is the small mutable cursor shared by the plan tool and
// subsequent tool dispatches in one Run. The durable plan remains authoritative;
// this cursor only correlates a dispatch with the plan version/step that was
// current when the call began.
type RunExecutionState struct {
	mu          sync.RWMutex
	planVersion int
	planStepID  string
}

type runExecutionStateKey struct{}

func NewRunExecutionState() *RunExecutionState { return &RunExecutionState{} }

func WithRunExecutionState(ctx context.Context, state *RunExecutionState) context.Context {
	if ctx == nil || state == nil {
		return ctx
	}
	return context.WithValue(ctx, runExecutionStateKey{}, state)
}

func UpdateRunExecutionPlan(ctx context.Context, version int, stepID string) {
	if ctx == nil {
		return
	}
	state, _ := ctx.Value(runExecutionStateKey{}).(*RunExecutionState)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.planVersion = version
	state.planStepID = stepID
	state.mu.Unlock()
}

func CurrentRunExecutionPlan(ctx context.Context) (version int, stepID string) {
	if ctx == nil {
		return 0, ""
	}
	state, _ := ctx.Value(runExecutionStateKey{}).(*RunExecutionState)
	if state == nil {
		return 0, ""
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.planVersion, state.planStepID
}
