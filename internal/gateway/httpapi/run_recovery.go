package httpapi

// Stuck-run recovery. A daemon kill/crash mid-run leaves task_runs rows in
// 'running' and their tasks in 'running' with nothing executing — phantom work
// in /tasks that never resolves. Recovery has two layers:
//
//  1. Boot sweep (gateway runner): the gateway.lock flock guarantees a single
//     daemon per control.db, so at boot every leftover 'running' run is dead;
//     the runner calls Store.MarkInterruptedRuns(0) before serving traffic.
//  2. Periodic sweep (this file): while the daemon runs, a ticker marks runs
//     whose heartbeat went stale (e.g. a worker goroutine died without
//     finalizing) — but never a run present in the in-memory active-run
//     registry, which is the source of truth for "actually executing".
//
// Both layers share the store-side invariant (see MarkInterruptedRuns): after
// a sweep, no task may remain 'running' with zero live runs.

import (
	"context"
	"sync"
	"time"

	"selfmind/internal/platform/log"
)

const (
	// runHeartbeatInterval is how often a live run refreshes
	// task_runs.heartbeat_at (see startRunHeartbeat). The staleness threshold
	// below is expressed as a multiple of it so the two can never drift apart.
	runHeartbeatInterval = 10 * time.Second

	// stuckRunSweepInterval is how often the daemon scans for heartbeat-dead
	// runs. Cheap (one indexed SELECT on an idle daemon), so a fixed constant
	// rather than configuration.
	stuckRunSweepInterval = time.Minute

	// staleRunHeartbeatThreshold: a 'running' run whose heartbeat is older than
	// this is considered dead. 12 missed heartbeats (2 minutes) tolerates
	// SQLite write contention and scheduler hiccups without ever racing a
	// healthy 10s heartbeat; the active-run registry exclusion is the hard
	// safety net on top.
	staleRunHeartbeatThreshold = 12 * runHeartbeatInterval
)

// activeRunIDs snapshots the run IDs currently registered as executing. The
// periodic sweep must never touch these, regardless of heartbeat age.
func (c *RunCoordinator) activeRunIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for _, active := range c.active {
		if active != nil && active.RunID != "" {
			ids = append(ids, active.RunID)
		}
	}
	return ids
}

// StartStuckRunSweeper starts the periodic stuck-run recovery loop with the
// production interval/threshold and returns a stop function. The gateway
// runner owns its lifecycle; eval and tests never start it implicitly.
func (d *Server) StartStuckRunSweeper(ctx context.Context) func() {
	return d.startStuckRunSweeper(ctx, stuckRunSweepInterval, staleRunHeartbeatThreshold)
}

// startStuckRunSweeper is the injectable-core of StartStuckRunSweeper so tests
// can drive a short interval. Stopping is idempotent.
func (d *Server) startStuckRunSweeper(ctx context.Context, interval, threshold time.Duration) func() {
	if d == nil || d.Control == nil || interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.sweepStuckRuns(threshold)
				// Same 60s cadence re-pushes pending approvals/clarifies that were
				// left uninformed once the CLI detaches (Fix 2). No-op when the
				// escrow threshold is unset (PendingNotifyAfter <= 0).
				d.sweepPendingNotifications(d.PendingNotifyAfter)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// sweepStuckRuns runs one recovery pass, shielding every run in the active
// registry from the heartbeat check.
func (d *Server) sweepStuckRuns(threshold time.Duration) {
	except := d.coordinator().activeRunIDs()
	recovered, err := d.Control.MarkInterruptedRuns(context.Background(), threshold, except...)
	if err != nil {
		log.Warn("gateway: stuck-run sweep failed", "error", err)
		return
	}
	if recovered > 0 {
		log.Warn("gateway: recovered stuck runs/tasks", "count", recovered, "stale_after", threshold.String())
	}
}

// sweepPendingNotifications is the escrow pass (Fix 2): it finds pending
// approvals/clarifies older than the threshold that were never notified and, for
// those whose person has since detached from the CLI, re-pushes them to the
// preferred IM. It is bounded (each list caps at 100) and idempotent — a push
// stamps notified_at only on success, so the next sweep is a no-op for already
// notified rows and a retry for failed ones. A zero threshold disables escrow.
func (d *Server) sweepPendingNotifications(threshold time.Duration) {
	if d == nil || d.Control == nil || threshold <= 0 {
		return
	}
	ctx := context.Background()
	cutoff := time.Now().Add(-threshold)
	coord := d.coordinator()
	if approvals, err := d.Control.ListPendingApprovalsForEscrow(ctx, cutoff); err != nil {
		log.Warn("gateway: escrow approval scan failed", "error", err)
	} else {
		for i := range approvals {
			coord.escrowApprovalNotification(ctx, &approvals[i])
		}
	}
	if clarifies, err := d.Control.ListPendingClarifiesForEscrow(ctx, cutoff); err != nil {
		log.Warn("gateway: escrow clarify scan failed", "error", err)
	} else {
		for i := range clarifies {
			coord.escrowClarifyNotification(ctx, &clarifies[i])
		}
	}
}
