package tools

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// Host execution excludes the person's other commands and file mutations.
// Isolated commands and file edits share the lane until a host call is waiting.
// The gate owns no permissions and is private to one daemon Runtime.
type executionLane struct {
	mu               sync.Mutex
	readers, writers int
	host             bool
	changed          chan struct{}
	queuedHosts      map[string]bool
	queuedOrder      []string
	yieldedHosts     map[string]bool
}
type executionLaneKey struct{ tenant, person string }
type executionLaneToken struct {
	runtime    *ExecutionRuntime
	key        executionLaneKey
	active     *atomic.Bool
	host       bool
	invocation executionLaneInvocation
}
type executionLaneContextKey struct{}
type executionLaneInvocation struct{ run, lease, call string }
type laneAcquisition struct {
	ref     string
	refresh func(context.Context) error
}
type executionLaneRefreshKey struct{}

// WithExecutionLaneRefresh installs a daemon-owned queue selector for durable
// polls which have no live Run scope. An exact scope overrides inherited context.
func WithExecutionLaneRefresh(ctx context.Context, refresh func(context.Context) error) context.Context {
	return context.WithValue(ctx, executionLaneRefreshKey{}, refresh)
}

const executionLaneGrace = 30 * time.Second

func (r *ExecutionRuntime) lane(key executionLaneKey) *executionLane {
	r.lanes.Lock()
	defer r.lanes.Unlock()
	if r.lanes.entries == nil {
		r.lanes.entries = make(map[executionLaneKey]*executionLane)
	}
	lane := r.lanes.entries[key]
	if lane == nil {
		lane = &executionLane{changed: make(chan struct{})}
		r.lanes.entries[key] = lane
	}
	return lane
}
func (l *executionLane) notify() { close(l.changed); l.changed = make(chan struct{}) }
func (l *executionLane) acquire(ctx context.Context, host bool, options ...laneAcquisition) (func(), error) {
	var option laneAcquisition
	if len(options) > 0 {
		option = options[0]
	}
	if option.refresh != nil {
		if err := option.refresh(ctx); err != nil {
			return nil, fmt.Errorf("inspect managed execution wait: %w", err)
		}
	}
	l.mu.Lock()
	if host {
		l.writers++
	}
	for {
		if ctx.Err() != nil {
			if host {
				l.writers--
				l.notify()
			}
			l.mu.Unlock()
			return nil, ctx.Err()
		}
		queued := len(l.queuedHosts) > 0
		hostTurn := !queued || (option.ref != "" && option.ref == l.firstQueuedHost())
		if !l.host && ((!host && l.writers == 0 && !queued) || (host && l.readers == 0 && hostTurn)) {
			if host {
				l.writers--
				l.host = true
				if option.ref != "" {
					l.yieldHost(option.ref)
				}
			} else {
				l.readers++
			}
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					if host {
						l.host = false
					} else {
						l.readers--
					}
					l.notify()
					l.mu.Unlock()
				})
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		var poll <-chan time.Time
		var timer *time.Timer
		if option.refresh != nil {
			timer = time.NewTimer(200 * time.Millisecond)
			poll = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-poll:
		}
		if timer != nil {
			timer.Stop()
		}
		var refreshErr error
		if ctx.Err() == nil && option.refresh != nil {
			refreshErr = option.refresh(ctx)
		}
		l.mu.Lock()
		if refreshErr != nil {
			if host {
				l.writers--
				l.notify()
			}
			l.mu.Unlock()
			return nil, fmt.Errorf("inspect managed execution wait: %w", refreshErr)
		}
	}
}

// ExecutionLaneReady is only queue readiness, never dispatch authority. Every
// actual dispatch acquires again, so a concurrent command cannot bypass the gate.
func (r *ExecutionRuntime) ExecutionLaneReady(tenant, person string, hostRef ...string) bool {
	if r == nil {
		return false
	}
	l := r.lane(executionLaneKey{tenant, person})
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(hostRef) > 0 && hostRef[0] != "" {
		return !l.host && l.readers == 0 && (l.firstQueuedHost() == "" || l.firstQueuedHost() == hostRef[0])
	}
	return !l.host && l.readers == 0 && l.writers == 0 && len(l.queuedHosts) == 0
}
func executionLaneIdentity(ctx context.Context, args map[string]interface{}, req *ExecutionRequest) (*ExecutionRuntime, executionLaneKey, bool) {
	r := executionRuntimeFromArgs(args)
	if r == nil {
		r = executionRuntimeFromContext(ctx)
	}
	if scope, ok := currentExecutionScopeAny(args); ok {
		return r, executionLaneKey{scope.TenantID, scope.PersonID}, scope.ParallelWork
	}
	if req != nil && req.Durable != nil {
		return r, executionLaneKey{req.Durable.TenantID, req.Durable.PersonID}, true
	}
	return r, executionLaneKey{}, false
}
func acquireManagedExecution(ctx context.Context, args map[string]interface{}, req *ExecutionRequest, host bool) (context.Context, func(), error) {
	r, key, required := executionLaneIdentity(ctx, args, req)
	var invocation executionLaneInvocation
	var option laneAcquisition
	if scope, ok := currentExecutionScopeAny(args); ok {
		invocation.run, invocation.lease = scope.RunID, scope.LeaseID
		option = laneAcquisition{scope.ExecutionWaitRef, scope.RefreshExecutionWaits}
	} else {
		option.refresh, _ = ctx.Value(executionLaneRefreshKey{}).(func(context.Context) error)
		if req != nil {
			invocation.run, invocation.lease = req.RunID, req.LeaseID
			if req.Durable != nil {
				invocation.run = req.Durable.ScratchKey
			}
		}
	}
	invocation.call = kernel.HumanWaitToolCallID(ctx)
	if invocation.call == "" {
		invocation.call = stringArg(args, "_tool_call_id")
	}
	if invocation.call == "" && req != nil {
		invocation.call = req.ToolCallID
	}
	if token, ok := ctx.Value(executionLaneContextKey{}).(executionLaneToken); ok && token.runtime == r && token.key == key && token.invocation == invocation && token.active.Load() {
		if host && !token.host {
			return ctx, nil, fmt.Errorf("approved execution lane boundary changed; host command was not started")
		}
		return ctx, func() {}, nil
	}
	if r == nil || key.person == "" {
		if required {
			return ctx, nil, newStableToolRecoveryError(fmt.Errorf("execution runtime is unavailable"), "execution_runtime_unavailable", "environment", "The managed execution was not started because its execution runtime is unavailable.", "Restore the daemon execution runtime before retrying.", "admission", "after_policy_change", "not_dispatched", false)
		}
		return ctx, func() {}, nil
	}
	release, err := r.lane(key).acquire(ctx, host, option)
	if err != nil {
		return ctx, nil, err
	}
	active := &atomic.Bool{}
	active.Store(true)
	return context.WithValue(WithExecutionRuntime(ctx, r), executionLaneContextKey{}, executionLaneToken{r, key, active, host, invocation}), func() { active.Store(false); release() }, nil
}

// ManagedExecutionMiddleware is innermost: effects have been checked and
// claimed before waiting here, and the Registry has not entered a tool body.
func ManagedExecutionMiddleware() ResultMiddleware {
	return func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			name := stringArg(args, "_tool_name")
			annotateEffectiveSandboxMode(args)
			// watch_external registers durable work and may ask for human input.
			// Its actual preflight and later polls acquire in the execution engine.
			managedExec := isExecTool(name) && name != "watch_external"
			if !managedExec && !isWriteTool(name) {
				return next(args)
			}
			ctx := contextFromArgs(args)
			host := managedExec && effectiveSandboxModeArg(args) == SandboxHost
			managed, release, err := waitForManagedExecution(ctx, args, host)
			if err != nil {
				return uninvokedExternalEffect(err)
			}
			defer release()
			// A host owner may have changed directories or symlinks while this
			// call waited. Recheck the exact approved arguments before entering
			// its body; normalization may not widen a previously approved call.
			if err := recheckManagedExecutionScope(args); err != nil {
				return uninvokedExternalEffect(err)
			}
			original, hadContext := args["_context"]
			args["_context"] = managed
			defer func() {
				if hadContext {
					args["_context"] = original
				} else {
					delete(args, "_context")
				}
			}()
			return next(args)
		}
	}
}

func recheckManagedExecutionScope(args map[string]interface{}) error {
	checked := make(map[string]interface{}, len(args))
	for key, value := range args {
		checked[key] = value
	}
	_, err := WorkspaceScopeMiddleware()(func(map[string]interface{}) (string, error) { return "", nil })(checked)
	if err == nil {
		for _, key := range []string{"path", "cwd", "patch"} {
			if stringArg(checked, key) != stringArg(args, key) {
				err = fmt.Errorf("the approved %s changed while waiting", key)
				break
			}
		}
	}
	if err != nil {
		return newStableToolRecoveryError(err, "execution_scope_changed", "environment", "The waiting call was not dispatched because its scoped path is no longer valid.", "Inspect the current workspace paths before requesting a new call.", "admission", "after_policy_change", "not_dispatched", false)
	}
	return nil
}

func waitForManagedExecution(ctx context.Context, args map[string]interface{}, host bool) (context.Context, func(), error) {
	scope, installed := currentExecutionScopeAny(args)
	if !installed || scope.ParkExecutionWait == nil {
		return acquireManagedExecution(ctx, args, nil, host)
	}
	grace := scope.ExecutionWaitGrace
	if grace <= 0 {
		grace = executionLaneGrace
	}
	waitCtx, cancel := context.WithTimeout(ctx, grace)
	managed, release, err := acquireManagedExecution(waitCtx, args, nil, host)
	cancel()
	if err == nil {
		// The timeout bounded acquisition, not the command which now owns the lane.
		token := managed.Value(executionLaneContextKey{})
		if token != nil {
			ctx = context.WithValue(WithExecutionRuntime(ctx, executionRuntimeFromArgs(args)), executionLaneContextKey{}, token)
		}
		return ctx, release, nil
	}
	if ctx.Err() != nil {
		return ctx, nil, ctx.Err()
	}
	if err != context.DeadlineExceeded {
		return ctx, nil, err
	}
	parked, parkErr := scope.ParkExecutionWait(ctx, stringArg(args, "_tool_name"), host)
	if parked {
		return ctx, nil, executionLanePause{}
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{Type: "agent.thinking", Content: "Waiting for the managed execution lane. Its checkpoint could not be parked safely, so this worker remains occupied; /stop cancels it.", Payload: map[string]interface{}{"phase": "execution_lane_wait", "worker_retained": true}})
	log.Warn("execution lane wait retains cancellable worker", "run_id", scope.RunID, "error", parkErr)
	return acquireManagedExecution(ctx, args, nil, host)
}

type executionLanePause struct{}

func (executionLanePause) Error() string {
	return "managed execution lane is occupied; this call was not dispatched"
}
func (executionLanePause) ToolRunPause() (string, string, bool) {
	return "execution_lane_wait", "Waiting for the managed execution lane. SelfMind will resume this exact work when it becomes available; the pending call was not dispatched.", false
}
func (executionLanePause) ToolRunPauseStatus() string { return "waiting_external" }
func (executionLanePause) ToolErrorCode() string      { return "execution_lane_wait" }
func (executionLanePause) ToolErrorCategory() string  { return "resource_wait" }
func (p executionLanePause) ModelSafeMessage() string { return p.Error() }
func (executionLanePause) ToolRecoveryHint() string {
	return "The daemon resumes the exact uninvoked tool batch. Do not replay completed effects."
}
func (executionLanePause) ToolFailurePhase() string { return "admission" }
func (executionLanePause) ToolRetryability() string { return "after_resource_release" }
func (executionLanePause) ToolEffectState() string  { return "not_dispatched" }
func (executionLanePause) ToolStateChanged() bool   { return false }

func (l *executionLane) yieldHost(ref string) {
	if ref == "" {
		return
	}
	delete(l.queuedHosts, ref)
	if l.yieldedHosts == nil {
		l.yieldedHosts = map[string]bool{}
	}
	l.yieldedHosts[ref] = true
}

// PreserveQueuedHostExecution carries writer intent across worker parking. It
// does not acquire the lane, and is reconciled from durable queue/checkpoint facts.
func (r *ExecutionRuntime) PreserveQueuedHostExecution(tenant, person, ref string) {
	if r == nil || ref == "" {
		return
	}
	l := r.lane(executionLaneKey{tenant, person})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queuedHosts == nil {
		l.queuedHosts = map[string]bool{}
	}
	if !l.queuedHosts[ref] {
		l.queuedOrder = append(l.queuedOrder, ref)
	}
	l.queuedHosts[ref] = true
	l.notify()
}
func (r *ExecutionRuntime) YieldQueuedHostExecution(tenant, person, ref string) {
	if r == nil || ref == "" {
		return
	}
	l := r.lane(executionLaneKey{tenant, person})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.yieldHost(ref)
	l.notify()
}
func (r *ExecutionRuntime) ReconcileQueuedHostExecution(tenant, person string, refs []string) {
	if r == nil {
		return
	}
	l := r.lane(executionLaneKey{tenant, person})
	l.mu.Lock()
	defer l.mu.Unlock()
	present := map[string]bool{}
	for _, ref := range refs {
		present[ref] = true
	}
	for ref := range l.yieldedHosts {
		if !present[ref] {
			delete(l.yieldedHosts, ref)
		}
	}
	queued := map[string]bool{}
	for ref := range present {
		if !l.yieldedHosts[ref] {
			queued[ref] = true
		}
	}
	changed := len(queued) != len(l.queuedHosts)
	for ref := range queued {
		changed = changed || !l.queuedHosts[ref]
	}
	l.queuedHosts = queued
	l.queuedOrder = append([]string(nil), refs...)
	if changed {
		l.notify()
	}
}
func yieldQueuedHostExecution(args map[string]interface{}) {
	if scope, ok := currentExecutionScopeAny(args); ok {
		scope.Runtime.YieldQueuedHostExecution(scope.TenantID, scope.PersonID, scope.ExecutionWaitRef)
	}
}

func (l *executionLane) firstQueuedHost() string {
	for _, ref := range l.queuedOrder {
		if l.queuedHosts[ref] {
			return ref
		}
	}
	return ""
}
