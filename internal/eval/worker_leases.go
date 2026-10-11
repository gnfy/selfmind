package eval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/runpool"
)

// workerLease is one Run's tenure on an agent worker, as the gateway reports
// it through its worker lease hook.
type workerLease struct {
	// turn is the 1-based case turn whose request started the Run; 0 for work
	// the daemon started itself, such as a drained queue item.
	turn   int
	worker int
	runID  string
	taskID string
	start  time.Time
	end    time.Time
	held   bool
	// endedBy says why a held lease ended; see holdEndReason. Only
	// holdStopped means the case stopped the Run.
	endedBy string
}

// Why a held lease ended.
const (
	holdStopped  = "stopped"
	holdDeadline = "deadline"
	holdStalled  = "stalled"
	holdReleased = "released"
)

// errHoldReleased ends a held Run that the case never stopped. It wraps
// context.Canceled so the gateway records a cancellation, not an interrupted
// model connection that schedules an automatic recovery Run.
var errHoldReleased = fmt.Errorf("%w: eval released a held worker because the case ended without stopping its Run", context.Canceled)

// holdEndReason classifies why a held Run's context ended. A stop cancels it
// with context.Canceled; the turn deadline, the run watchdog and a gateway
// shutdown cancel the same context with their own cause.
func holdEndReason(cause error) string {
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return holdDeadline
	case errors.Is(cause, runpool.ErrStalled):
		return holdStalled
	case errors.Is(cause, context.Canceled):
		return holdStopped
	case cause == nil:
		return "an unknown cancellation"
	default:
		return "cancellation (" + cause.Error() + ")"
	}
}

// workerLeaseLog records every lease of a case and holds the leases of
// hold_worker turns until their Runs are stopped.
type workerLeaseLog struct {
	mu        sync.Mutex
	leases    []*workerLease
	holds     map[int]chan struct{}
	teardown  chan struct{}
	closeOnce sync.Once
}

func newWorkerLeaseLog() *workerLeaseLog {
	return &workerLeaseLog{holds: map[int]chan struct{}{}, teardown: make(chan struct{})}
}

type evalTurnKey struct{}

// withEvalTurn marks a turn's requests so the lease hook can name the turn.
// The gateway keeps context values on the Run it derives from the request.
func withEvalTurn(ctx context.Context, turn int) context.Context {
	return context.WithValue(ctx, evalTurnKey{}, turn)
}

func evalTurnFromContext(ctx context.Context) int {
	turn, _ := ctx.Value(evalTurnKey{}).(int)
	return turn
}

// expectHold makes the next lease of turn a held one. The returned channel
// closes when that turn's Run has leased its worker.
func (l *workerLeaseLog) expectHold(turn int) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	leased := make(chan struct{})
	l.holds[turn] = leased
	return leased
}

// hook is the gateway's worker lease hook.
func (l *workerLeaseLog) hook(ctx context.Context, worker int) (func(), error) {
	lease := &workerLease{turn: evalTurnFromContext(ctx), worker: worker, start: time.Now()}
	if scope, ok := kernel.ToolInvocationScopeFromContext(ctx); ok {
		lease.runID, lease.taskID = scope.RunID, scope.TaskID
	}
	l.mu.Lock()
	l.leases = append(l.leases, lease)
	leased, hold := l.holds[lease.turn]
	if hold {
		delete(l.holds, lease.turn)
		lease.held = true
	}
	l.mu.Unlock()
	if !hold {
		return func() { l.finish(lease, "") }, nil
	}
	close(leased)
	select {
	case <-ctx.Done():
		l.finish(lease, holdEndReason(context.Cause(ctx)))
		return nil, ctx.Err()
	case <-l.teardown:
		l.finish(lease, holdReleased)
		return nil, errHoldReleased
	}
}

func (l *workerLeaseLog) finish(lease *workerLease, endedBy string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lease.end = time.Now()
	lease.endedBy = endedBy
}

// releaseHolds ends every lease still held, so a case that forgot to stop a
// Run cannot keep its worker past the case.
func (l *workerLeaseLog) releaseHolds() {
	l.closeOnce.Do(func() { close(l.teardown) })
}

// snapshot copies the leases recorded so far.
func (l *workerLeaseLog) snapshot() []workerLease {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]workerLease, 0, len(l.leases))
	for _, lease := range l.leases {
		out = append(out, *lease)
	}
	return out
}

// turnLease returns the first lease a turn's request took.
func (l *workerLeaseLog) turnLease(turn int) (workerLease, bool) {
	for _, lease := range l.snapshot() {
		if lease.turn == turn {
			return lease, true
		}
	}
	return workerLease{}, false
}

// leasedWorkers counts the distinct workers that served a lease.
func leasedWorkers(leases []workerLease) int {
	workers := map[int]bool{}
	for _, lease := range leases {
		workers[lease.worker] = true
	}
	return len(workers)
}

// peakLeaseOverlap is the largest number of leases held at the same moment.
// A lease that is still open counts until now; a lease that ends exactly when
// another starts does not overlap it.
func peakLeaseOverlap(leases []workerLease) int {
	type edge struct {
		at    time.Time
		delta int
	}
	edges := make([]edge, 0, 2*len(leases))
	for _, lease := range leases {
		edges = append(edges, edge{at: lease.start, delta: 1})
		if !lease.end.IsZero() {
			edges = append(edges, edge{at: lease.end, delta: -1})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].at.Equal(edges[j].at) {
			return edges[i].delta < edges[j].delta
		}
		return edges[i].at.Before(edges[j].at)
	})
	current, peak := 0, 0
	for _, e := range edges {
		current += e.delta
		if current > peak {
			peak = current
		}
	}
	return peak
}
