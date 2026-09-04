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
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/delivery"
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
		// The earlier MarkInterruptedRuns boot sweep runs before Delivery is
		// available. This worker itself starts only after Delivery is installed,
		// so durable interruption and retention events can be surfaced now.
		d.sweepRecoveryNotifications()
		d.sweepApprovalRetention()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.sweepStuckRuns(threshold)
				d.recoverApprovalContinuations(ctx, true)
				d.sweepRecoveryNotifications()
				// Same 60s cadence re-pushes pending approvals/clarifies that were
				// left uninformed once the CLI detaches (Fix 2). No-op when the
				// escrow threshold is unset (PendingNotifyAfter <= 0).
				d.sweepPendingNotifications(d.PendingNotifyAfter)
				d.sweepApprovalRetention()
				// Run scratch was only swept at boot, so a daemon that stays up
				// for weeks never reclaimed it: the per-run soft quota cannot
				// help, because a hundred finished runs holding a gigabyte each
				// never trip a per-lease limit.
				d.sweepRunScratch()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// recoverApprovalContinuations closes the only crash window left after a
// human decision is durable: the daemon may die after recording approved or
// rejected but before the blocked run consumes it. The original run is gone,
// so recovery creates one task-pinned continuation and lets the new run
// re-evaluate the step. Approval authority itself is not carried in prose; an
// unclaimed approval receives a one-shot, exact-action token in control.db
// which the execution middleware consumes atomically.
func (d *Server) recoverApprovalContinuations(ctx context.Context, drain bool) int {
	if d == nil || d.Control == nil {
		return 0
	}
	approvals, err := d.Control.ListRecoverableApprovalDecisions(ctx, 100)
	if err != nil {
		log.Warn("gateway: recoverable approval decision scan failed", "error", err)
		return 0
	}
	recovered := 0
	people := map[string]*control.IdentityContext{}
	for i := range approvals {
		approval := approvals[i]
		task, taskErr := d.Control.GetTask(ctx, approval.TenantID, approval.TaskID)
		if taskErr != nil || task == nil {
			log.Warn("gateway: recoverable approval has no task", "approval_id", approval.ID, "task_id", approval.TaskID, "error", taskErr)
			continue
		}
		channel := fallback(approval.ApprovedChannel, approval.RequestedChannel)
		route := d.routeIdentityForPerson(ctx, approval.TenantID, approval.PersonID, channel, "", nil)
		var executionRoots []executionenv.RootBinding
		sourceRun, runErr := d.Control.GetRun(ctx, approval.TenantID, approval.RunID)
		if runErr != nil {
			log.Warn("gateway: recoverable approval run scope lookup failed", "approval_id", approval.ID, "run_id", approval.RunID, "error", runErr)
			continue
		}
		if sourceRun != nil {
			executionRoots = executionenv.CloneRootBindings(sourceRun.ExecutionRoots)
		}
		queued, created, enqueueErr := d.Control.EnqueueRecoveredApprovalContinuation(ctx, approval.ID, control.QueuedTask{
			TenantID:       approval.TenantID,
			PersonID:       approval.PersonID,
			Platform:       route.Platform,
			PlatformUserID: route.PlatformUserID,
			Channel:        fallback(channel, route.Platform),
			Content:        parkedApprovalDecisionContent(approval.Status, approval.DecisionNote),
			WorkspaceID:    recoveryWorkspaceID(sourceRun, task),
			ExecutionRoots: executionRoots,
			TaskID:         task.ID,
			ApprovalID:     approval.ID,
			IdempotencyKey: "approval-resume:" + approval.ID,
			Class:          control.QueueClassForeground,
		})
		if enqueueErr != nil {
			log.Warn("gateway: approval continuation recovery failed", "approval_id", approval.ID, "error", enqueueErr)
			continue
		}
		if !created || queued == nil {
			continue
		}
		recovered++
		key := approval.TenantID + "|" + approval.PersonID
		people[key] = route
	}
	if recovered > 0 {
		log.Warn("gateway: recovered approval continuations", "count", recovered)
	}
	if drain {
		for _, identity := range people {
			d.coordinator().drainQueue(identity)
		}
	}
	return recovered
}

func (d *Server) sweepApprovalRetention() {
	if d == nil || d.Control == nil {
		return
	}
	ctx := context.Background()
	archived, err := d.Control.ArchiveStaleParkedApprovals(ctx, control.ParkedApprovalRetention)
	if err != nil {
		log.Warn("gateway: parked approval retention sweep failed", "error", err)
		return
	}
	for i := range archived {
		approval := &archived[i]
		if _, eventErr := d.Control.AppendApprovalResolutionEvent(ctx, approval, approval.RequestedChannel, "parked approval retention elapsed"); eventErr != nil {
			log.Warn("gateway: append approval.archived event failed", "approval_id", approval.ID, "error", eventErr)
		}
		d.notifyApprovalResolutionElsewhere(ctx, &control.IdentityContext{
			TenantID: approval.TenantID, PersonID: approval.PersonID, Platform: "system",
		}, approval, "")
	}
}

// sweepRecoveryNotifications turns durable daemon-recovery events into one
// presence-aware notification. It marks an event only after delivery was
// actually enqueued, so an offline/unbound endpoint remains retryable.
func (d *Server) sweepRecoveryNotifications() {
	if d == nil || d.Control == nil || d.Delivery == nil {
		return
	}
	ctx := context.Background()
	items, err := d.Control.ListPendingRecoveryNotifications(ctx, 50)
	if err != nil {
		log.Warn("gateway: recovery notification scan failed", "error", err)
		return
	}
	for _, item := range items {
		if scheduled, scheduleErr := d.scheduleAutomaticRunRecovery(ctx, item, true); scheduleErr != nil {
			log.Warn("gateway: automatic run recovery scheduling failed", "run", item.RunID, "error", scheduleErr)
		} else if scheduled {
			continue
		}
		identity := &control.IdentityContext{TenantID: item.TenantID, PersonID: item.PersonID, Platform: "cli"}
		content := recoveryNotificationContent(item.Title,
			d.recoveryHandoffForRun(ctx, item.TenantID, item.PersonID, item.RunID))
		if d.coordinator().routePendingNotification(ctx, identity, item.Channel, delivery.Message{
			TenantID: item.TenantID, PersonID: item.PersonID, TaskID: item.TaskID, RunID: item.RunID,
			Content: content, Kind: "recovery",
		}) {
			if err := d.Control.MarkRecoveryNotificationSent(ctx, item); err != nil {
				log.Warn("gateway: recovery notification marker failed", "run", item.RunID, "error", err)
			}
		}
	}
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
	// A maintenance job stuck 'running' means the daemon died mid-pass; the
	// claim CAS makes reset-then-reclaim safe, so return it to pending.
	if reset, err := d.Control.ResetStaleMaintenanceJobs(context.Background(), threshold); err == nil && reset > 0 {
		log.Warn("gateway: reset stale maintenance jobs", "count", reset)
	}
}

// sweepPendingNotifications is the escrow pass: it finds every un-notified
// pending approval/clarify and escalates it when either the CLI has detached or
// the request has remained unanswered past threshold. It is bounded (each list
// caps at 100) and idempotent — a push
// stamps notified_at only on success, so the next sweep is a no-op for already
// notified rows and a retry for failed ones. A zero threshold disables escrow.
func (d *Server) sweepPendingNotifications(threshold time.Duration) {
	if d == nil || d.Control == nil || threshold <= 0 {
		return
	}
	ctx := context.Background()
	// Presence decides whether a fresh row may go now, so the store must return
	// fresh rows too. Age gating belongs below, beside the presence check.
	cutoff := time.Now().Add(time.Second)
	coord := d.coordinator()
	if approvals, err := d.Control.ListPendingApprovalsForEscrow(ctx, cutoff); err != nil {
		log.Warn("gateway: escrow approval scan failed", "error", err)
	} else {
		for i := range approvals {
			coord.escrowApprovalNotification(ctx, &approvals[i], threshold)
		}
	}
	if clarifies, err := d.Control.ListPendingClarifiesForEscrow(ctx, cutoff); err != nil {
		log.Warn("gateway: escrow clarify scan failed", "error", err)
	} else {
		for i := range clarifies {
			coord.escrowClarifyNotification(ctx, &clarifies[i], threshold)
		}
	}
}

// runScratchRetention is how long a finished run's scratch stays inspectable
// before it is reclaimed.
const runScratchRetention = 24 * time.Hour

// sweepRunScratch reclaims scratch from runs that finished more than the
// retention window ago. A lease belonging to a run that is still live is never
// removed, no matter how old the directory looks: the age of the directory says
// nothing about whether the run is still writing to it.
func (d *Server) sweepRunScratch() {
	if d == nil || executionenv.RuntimeRoot() == "" {
		return
	}
	active := d.activeLeaseIDs()
	removed, err := executionenv.SweepExpiredScratch(runScratchRetention, func(leaseID string) bool {
		return active[leaseID]
	}, time.Now())
	if err != nil {
		log.Debug("run scratch sweep failed", "error", err)
		return
	}
	if removed > 0 {
		log.Info("reclaimed expired run scratch", "count", removed, "retention", runScratchRetention)
	}
}

// activeLeaseIDs collects the leases of runs that are currently executing, so
// their scratch is never reclaimed underneath them.
func (d *Server) activeLeaseIDs() map[string]bool {
	active := map[string]bool{}
	if d == nil || d.Control == nil {
		return active
	}
	for _, status := range d.coordinator().activeRunStatuses() {
		if strings.TrimSpace(status.RunID) == "" {
			continue
		}
		lease, err := d.Control.GetExecutionLeaseByRun(context.Background(), d.DefaultTenantID, status.RunID)
		if err != nil || lease == nil {
			continue
		}
		active[lease.ID] = true
	}
	// A durable watch keys its scratch by watch id and may be mid-check or
	// waiting for its next poll, so any watch that is not finished counts as
	// active. Its scratch holds the credential overlay every poll reuses.
	watches, err := d.Control.ListUnfinalizedExternalWatches(context.Background(),
		time.Now().Add(-30*24*time.Hour), 500)
	if err == nil {
		for _, watch := range watches {
			active[watch.ID] = true
		}
	}
	return active
}

// recoveryWorkspaceID resolves the workspace re-enqueued work must execute in.
// The Run is the authority: execution roots already come from it, and taking
// the workspace from the Task made a display-and-grouping row decide execution
// scope. The Task fallback covers only rows created before runs carried the
// scope, and it goes away with Task itself.
func recoveryWorkspaceID(run *control.Run, task *control.Task) string {
	if run != nil && strings.TrimSpace(run.WorkspaceID) != "" {
		return run.WorkspaceID
	}
	if task != nil {
		return task.WorkspaceID
	}
	return ""
}
