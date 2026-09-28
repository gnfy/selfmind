package kernel

import (
	"context"
	"strings"
	"sync"
	"time"
)

type delegationNamespaceKey struct{}

// WithDelegationNamespace names the sub-execution the next fork creates: the
// delegating tool call's ID, plus the goal's index in a batch. The name is
// deterministic, so replayed runs keep their identifiers.
func WithDelegationNamespace(ctx context.Context, namespace string) context.Context {
	namespace = strings.TrimSpace(namespace)
	if ctx == nil || namespace == "" {
		return ctx
	}
	return context.WithValue(ctx, delegationNamespaceKey{}, namespace)
}

// DelegationNamespace is the current delegation's name, or "" outside one.
func DelegationNamespace(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	namespace, _ := ctx.Value(delegationNamespaceKey{}).(string)
	return namespace
}

type turnPauseReportKey struct{}

// TurnPause is a human wait that ended a turn: a tool paused the run on the
// person, whose answer, not the model, decides what happens next.
type TurnPause struct {
	Reason       string
	Message      string
	NeedApproval bool
}

func (p *TurnPause) Error() string { return p.Message }

// ToolRunPause makes the wait the delegating tool's own, so the parent run
// parks on the same human wait instead of reading it as a finished report.
func (p *TurnPause) ToolRunPause() (string, string, bool) {
	return p.Reason, p.Message, p.NeedApproval
}

type turnPauseReport struct {
	mu    sync.Mutex
	pause *TurnPause
}

// WithTurnPauseReport lets the caller of a nested turn learn that it ended on
// a human wait. The turn's answer is then only the wait's notice.
func WithTurnPauseReport(ctx context.Context) (context.Context, func() *TurnPause) {
	report := &turnPauseReport{}
	return context.WithValue(ctx, turnPauseReportKey{}, report), func() *TurnPause {
		report.mu.Lock()
		defer report.mu.Unlock()
		return report.pause
	}
}

func reportTurnPause(ctx context.Context, pause TurnPause) {
	if report, ok := ctx.Value(turnPauseReportKey{}).(*turnPauseReport); ok {
		report.mu.Lock()
		report.pause = &pause
		report.mu.Unlock()
	}
}

// ForkDelegationContext preserves the parent's lifetime and execution
// authority without inheriting loop-local strategy, steering, checkpoint, or
// deferred-tool activation state. Each sub-agent therefore gets a fresh loop
// while its tools remain scoped to the parent run and workspace.
func ForkDelegationContext(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	return delegationContext{parent: parent, namespace: DelegationNamespace(parent)}
}

type delegationContext struct {
	parent    context.Context
	namespace string
}

func (c delegationContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c delegationContext) Done() <-chan struct{}       { return c.parent.Done() }
func (c delegationContext) Err() error                  { return c.parent.Err() }

func (c delegationContext) Value(key any) any {
	switch key.(type) {
	case toolLedgerKey:
		// The sub-agent records its calls in the parent run's ledger, under its
		// own namespace: its call IDs restart with its loop and may repeat the
		// parent's.
		if ledger, ok := c.parent.Value(key).(ToolLedger); ok && ledger != nil && c.namespace != "" {
			return namespacedToolLedger{inner: ledger, namespace: c.namespace}
		}
		return c.parent.Value(key)
	case delegationNamespaceKey:
		return c.namespace
	// The event channel is deliberately absent: a sub-agent's own stream and
	// turn events are not the parent's, so the delegation forwards only what
	// the parent run keeps.
	case workspaceContextKey,
		taskRuntimeContextKey,
		runtimeContextBundleKey,
		toolInvocationScopeContextKey,
		toolArtifactSinkKey,
		skillRuntimeContextKey:
		return c.parent.Value(key)
	default:
		return nil
	}
}

// namespacedToolLedger keeps a delegated loop's calls distinct from the
// parent's in the run ledger they share.
type namespacedToolLedger struct {
	inner     ToolLedger
	namespace string
}

func (l namespacedToolLedger) callID(toolCallID string) string {
	return l.namespace + "/" + toolCallID
}

func (l namespacedToolLedger) ClaimDispatch(ctx context.Context, entry ToolLedgerEntry) (ToolDispatchDecision, error) {
	entry.ToolCallID = l.callID(entry.ToolCallID)
	entry.EffectID = ToolEffectID(entry.RunID, entry.ToolCallID)
	return l.inner.ClaimDispatch(ctx, entry)
}

func (l namespacedToolLedger) RecordOutcome(ctx context.Context, runID, toolCallID string, ok bool) error {
	return l.inner.RecordOutcome(ctx, runID, l.callID(toolCallID), ok)
}

func (l namespacedToolLedger) RecordOutcomeWithRef(ctx context.Context, runID, toolCallID string, ok bool, resultRef string) error {
	if recorder, supported := l.inner.(ToolLedgerOutcomeRecorder); supported {
		return recorder.RecordOutcomeWithRef(ctx, runID, l.callID(toolCallID), ok, resultRef)
	}
	return l.inner.RecordOutcome(ctx, runID, l.callID(toolCallID), ok)
}

// delegatedCallIDPrefix prefixes the call IDs a delegated loop generates for a
// provider that sends none, so they stay unique within the parent's run. It
// keeps to the characters every provider accepts in a call ID.
func delegatedCallIDPrefix(ctx context.Context) string {
	namespace := DelegationNamespace(ctx)
	if namespace == "" {
		return ""
	}
	return "sub-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, namespace) + "-"
}
