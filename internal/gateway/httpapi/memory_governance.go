package httpapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

const (
	memoryGovernanceStartupGrace = 30 * time.Second
	memoryReadinessRetryDelay    = 5 * time.Second
	memoryGovernanceRetryDelay   = 10 * time.Minute
	// Backlog catch-up is intentionally slower than foreground/provider
	// recovery. At the default batch of eight this bounds a continuously idle
	// person to at most 48 cluster judgements per day while still draining a
	// historical backlog instead of waiting 24 hours between batches.
	memoryGovernanceBacklogRetryDelay = 4 * time.Hour
	memoryGovernanceMinimumWake       = time.Second
	// memoryGovernanceMaxRetryDelay caps the escalating backoff. A pass that
	// keeps crashing must slow down instead of re-burning model budget at the
	// base retry rate forever.
	memoryGovernanceMaxRetryDelay = 6 * time.Hour
)

// memoryGovernanceBackoff scales the retry clock with the durable failure
// counter (10m, 20m, 40m, … capped). consecutive_failures is only meaningful
// because interrupted passes are reconciled into failures at startup; before
// that a crash loop stayed at zero and retried at the base delay forever.
func memoryGovernanceBackoff(consecutiveFailures int) time.Duration {
	delay := memoryGovernanceRetryDelay
	for i := 1; i < consecutiveFailures && delay < memoryGovernanceMaxRetryDelay; i++ {
		delay *= 2
	}
	if delay > memoryGovernanceMaxRetryDelay {
		delay = memoryGovernanceMaxRetryDelay
	}
	return delay
}

// MemoryConsolidator runs one background self-organization pass over one
// person's memory partition. Implementations live in internal/app so the
// gateway stays provider- and storage-agnostic; RunGovernanceOnce must be safe
// to call repeatedly (cluster-level checkpointing) and must never touch pinned
// or user-confirmed memory.
type MemoryConsolidator interface {
	RunGovernanceOnce(ctx context.Context, personID string) (MemoryGovernanceResult, error)
	Interval() time.Duration
	PauseWhileRunActive() bool
	Mode() string
}

// MemoryGovernanceResult distinguishes a complete current-version scan from
// one bounded batch. Returning nil error is not enough: deadline and batch
// limits are healthy partial progress, not permission to defer for 24 hours.
type MemoryGovernanceResult struct {
	CandidateGroups int
	Judged          int
	Remaining       int
	Complete        bool
	StopReason      string
}

// StartMemoryGovernance starts the durable consolidation scheduler. The first
// wake happens after a short startup grace: overdue work catches up after a
// restart, while a future next_due_at survives restarts without paying twice.
// Foreground work always wins and defers due work to a short retry rather than
// resetting the full consolidation interval.
func (d *Server) StartMemoryGovernance(ctx context.Context) func() {
	if d == nil || d.MemoryConsolidator == nil || d.Control == nil {
		return func() {}
	}
	if !d.modelReadyForWork() {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	// The daemon is single-instance, so a row still marked in flight belongs to
	// a process that died mid-pass. Reconcile before the first wake so the
	// failure is counted and backed off instead of re-running immediately.
	reconcileAt := time.Now()
	if reconciled, err := d.Control.ReconcileInterruptedMemoryGovernance(ctx, reconcileAt,
		reconcileAt.Add(memoryGovernanceRetryDelay)); err != nil {
		log.Warn("memory governance: reconcile interrupted passes failed", "error", err)
	} else if reconciled > 0 {
		log.Warn("memory governance: interrupted pass(es) recorded as failures",
			"partitions", reconciled, "retry_in", memoryGovernanceRetryDelay)
	}
	go func() {
		delay := memoryGovernanceStartupGrace
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				delay = d.runMemoryGovernancePassAt(ctx, time.Now())
				if delay < memoryGovernanceMinimumWake {
					delay = memoryGovernanceMinimumWake
				}
				timer.Reset(delay)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (d *Server) runMemoryGovernancePass(ctx context.Context) time.Duration {
	return d.runMemoryGovernancePassAt(ctx, time.Now())
}

func (d *Server) runMemoryGovernancePassAt(ctx context.Context, now time.Time) time.Duration {
	if !d.modelReadyForWork() {
		return memoryReadinessRetryDelay
	}
	interval := d.MemoryConsolidator.Interval()
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	partitions, err := d.Control.ListPersonPartitions(ctx)
	if err != nil {
		log.Warn("memory governance: person listing failed", "error", err)
		return memoryGovernanceRetryDelay
	}
	if len(partitions) == 0 {
		return interval
	}
	for _, partition := range partitions {
		if err := d.Control.EnsureMemoryGovernanceSchedule(ctx, partition.TenantID, partition.PersonID, now); err != nil {
			log.Warn("memory governance: initialize schedule failed", "person", partition.PersonID, "error", err)
			return memoryGovernanceRetryDelay
		}
	}

	pauseForForeground := d.MemoryConsolidator.PauseWhileRunActive()
	if pauseForForeground && len(d.coordinator().activeRunIDs()) > 0 {
		d.deferDueMemoryGovernance(ctx, partitions, now, "foreground_run_active")
		log.Info("memory governance: due work deferred, foreground run active", "retry_in", memoryGovernanceRetryDelay)
		return memoryGovernanceRetryDelay
	}

	nextWake := interval
	for i, partition := range partitions {
		if ctx.Err() != nil {
			return memoryGovernanceRetryDelay
		}
		schedule, ok, err := d.Control.MemoryGovernanceScheduleForPerson(ctx, partition.TenantID, partition.PersonID)
		if err != nil || !ok {
			log.Warn("memory governance: read schedule failed", "person", partition.PersonID, "error", err)
			return memoryGovernanceRetryDelay
		}
		if schedule.NextDueAt.After(now) {
			nextWake = soonerDuration(nextWake, schedule.NextDueAt.Sub(now))
			continue
		}
		// Re-check before each partition: a run that started mid-pass pauses
		// the remainder and records a short, durable retry clock.
		if pauseForForeground && len(d.coordinator().activeRunIDs()) > 0 {
			d.deferDueMemoryGovernance(ctx, partitions[i:], now, "foreground_run_started")
			log.Info("memory governance: pass paused, foreground run started", "retry_in", memoryGovernanceRetryDelay)
			return soonerDuration(nextWake, memoryGovernanceRetryDelay)
		}
		// The attempt carries a crash lease: if this process dies before an
		// outcome is recorded, next_due_at is already pushed out by the backoff
		// instead of staying overdue and re-running after the startup grace.
		crashRetry := memoryGovernanceBackoff(schedule.ConsecutiveFailure + 1)
		if err := d.Control.RecordMemoryGovernanceAttempt(ctx, partition.TenantID, partition.PersonID, now, now.Add(crashRetry)); err != nil {
			log.Warn("memory governance: record attempt failed", "person", partition.PersonID, "error", err)
			return memoryGovernanceRetryDelay
		}
		passStarted := time.Now()
		passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		result, runErr := d.MemoryConsolidator.RunGovernanceOnce(passCtx, partition.PersonID)
		cancel()
		completedAt := now.Add(time.Since(passStarted))
		if runErr != nil {
			retryIn := memoryGovernanceBackoff(schedule.ConsecutiveFailure + 1)
			nextDue := completedAt.Add(retryIn)
			if err := d.Control.RecordMemoryGovernanceFailure(ctx, partition.TenantID, partition.PersonID, runErr.Error(), completedAt, nextDue); err != nil {
				log.Warn("memory governance: record failure failed", "person", partition.PersonID, "error", err)
			}
			log.Warn("memory governance: pass failed", "person", partition.PersonID, "error", runErr,
				"consecutive_failures", schedule.ConsecutiveFailure+1, "retry_in", retryIn)
			nextWake = soonerDuration(nextWake, retryIn)
			continue
		}
		if !result.Complete || result.Remaining > 0 {
			nextDue := completedAt.Add(memoryGovernanceBacklogRetryDelay)
			reason := strings.TrimSpace(result.StopReason)
			if reason == "" {
				reason = "backlog_remaining"
			}
			reason = fmt.Sprintf("%s:remaining=%d:judged=%d", reason, max(result.Remaining, 0), max(result.Judged, 0))
			if err := d.Control.RecordMemoryGovernancePartial(ctx, partition.TenantID, partition.PersonID, reason, completedAt, nextDue); err != nil {
				log.Warn("memory governance: record partial progress failed", "person", partition.PersonID, "error", err)
				return memoryGovernanceRetryDelay
			}
			log.Info("memory governance: bounded progress recorded", "person", partition.PersonID,
				"candidates", result.CandidateGroups, "judged", result.Judged, "remaining", result.Remaining,
				"reason", result.StopReason, "retry_in", memoryGovernanceBacklogRetryDelay)
			nextWake = soonerDuration(nextWake, nextDue.Sub(now))
			continue
		}
		nextDue := completedAt.Add(interval)
		if err := d.Control.RecordMemoryGovernanceSuccess(ctx, partition.TenantID, partition.PersonID, completedAt, nextDue); err != nil {
			log.Warn("memory governance: record success failed", "person", partition.PersonID, "error", err)
			return memoryGovernanceRetryDelay
		}
		nextWake = soonerDuration(nextWake, nextDue.Sub(now))
	}
	return nextWake
}

func (d *Server) deferDueMemoryGovernance(ctx context.Context, partitions []control.PersonPartition, now time.Time, reason string) {
	for _, partition := range partitions {
		schedule, ok, err := d.Control.MemoryGovernanceScheduleForPerson(ctx, partition.TenantID, partition.PersonID)
		if err != nil || !ok || schedule.NextDueAt.After(now) {
			continue
		}
		if err := d.Control.RecordMemoryGovernanceDeferred(ctx, partition.TenantID, partition.PersonID, reason, now, now.Add(memoryGovernanceRetryDelay)); err != nil {
			log.Warn("memory governance: record deferral failed", "person", partition.PersonID, "error", err)
		}
	}
}

func soonerDuration(current, candidate time.Duration) time.Duration {
	if candidate <= 0 {
		return memoryGovernanceMinimumWake
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}
