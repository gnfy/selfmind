package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"selfmind/internal/tools/sandbox"
	"strings"
	"sync"
	"sync/atomic"
)

// ExecutionRuntime owns daemon-lifetime execution state. Requests borrow it;
// stopping another runtime cannot replace private paths or steal process receipts.
// It deliberately contains no provider, permissions, environment or credentials.
type ExecutionRuntime struct {
	runtimeRoot  string
	instanceName string
	private      sandbox.PrivateState
	recorder     ProcessGroupRecorder
	commands     struct {
		sync.Mutex
		inFlight int
	}
	lastSweep atomic.Pointer[HostStateSweep]
	lanes     struct {
		sync.Mutex
		entries map[executionLaneKey]*executionLane
	}
}

func NewExecutionRuntime(dataDir, runtimeRoot string, private sandbox.PrivateState, recorder ProcessGroupRecorder) *ExecutionRuntime {
	if runtimeRoot = strings.TrimSpace(runtimeRoot); runtimeRoot != "" {
		if abs, err := filepath.Abs(runtimeRoot); err == nil {
			runtimeRoot = abs
		}
	}
	name := "default"
	if dataDir = strings.TrimSpace(dataDir); dataDir != "" {
		if abs, err := filepath.Abs(dataDir); err == nil {
			dataDir = abs
		}
		sum := sha256.Sum256([]byte(filepath.Clean(dataDir)))
		name = hex.EncodeToString(sum[:8])
	}
	return &ExecutionRuntime{runtimeRoot: runtimeRoot, instanceName: name, private: copyPrivateState(private), recorder: recorder}
}

func (r *ExecutionRuntime) PrivateState() sandbox.PrivateState {
	if r == nil {
		return sandbox.PrivateState{}
	}
	return copyPrivateState(r.private)
}

type executionRuntimeContextKey struct{}

func WithExecutionRuntime(ctx context.Context, runtime *ExecutionRuntime) context.Context {
	return context.WithValue(ctx, executionRuntimeContextKey{}, runtime)
}

func executionRuntimeFromContext(ctx context.Context) *ExecutionRuntime {
	if ctx == nil {
		return nil
	}
	if runtime, ok := ctx.Value(executionRuntimeContextKey{}).(*ExecutionRuntime); ok && runtime != nil {
		return runtime
	}
	if scope, ok := currentExecutionScopeAny(map[string]interface{}{"_context": ctx}); ok {
		return scope.Runtime
	}
	return nil
}

func executionRuntimeFromArgs(args map[string]interface{}) *ExecutionRuntime {
	// An exact invocation belongs to its registered runtime, even when the
	// caller inherited a context from a different daemon's background worker.
	if scope, ok := currentExecutionScopeAny(args); ok && scope.Runtime != nil {
		return scope.Runtime
	}
	if ctx, ok := args["_context"].(context.Context); ok {
		if runtime := executionRuntimeFromContext(ctx); runtime != nil {
			return runtime
		}
	}
	return nil
}
