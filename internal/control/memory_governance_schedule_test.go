package control

import (
	"context"
	"testing"
	"time"
)

func TestMemoryGovernanceSchedulePersistsLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "scheduler-owner", "Scheduler Owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	if err := store.EnsureMemoryGovernanceSchedule(ctx, identity.TenantID, identity.PersonID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordMemoryGovernanceAttempt(ctx, identity.TenantID, identity.PersonID, now, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordMemoryGovernanceFailure(ctx, identity.TenantID, identity.PersonID, "provider unavailable", now.Add(time.Minute), now.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	schedule, ok, err := store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !ok {
		t.Fatalf("schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
	if schedule.LastOutcome != MemoryGovernanceOutcomeFailed || schedule.ConsecutiveFailure != 1 || !schedule.NextDueAt.Equal(now.Add(11*time.Minute)) {
		t.Fatalf("failed schedule=%+v", schedule)
	}
	if err := store.RecordMemoryGovernanceSuccess(ctx, identity.TenantID, identity.PersonID, now.Add(2*time.Minute), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	schedule, ok, err = store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !ok || schedule.LastOutcome != MemoryGovernanceOutcomeSucceeded || schedule.ConsecutiveFailure != 0 {
		t.Fatalf("successful schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
	if schedule.LastError != "" || !schedule.LastSuccessAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("success did not clear failure state: %+v", schedule)
	}
	if err := store.RecordMemoryGovernancePartial(ctx, identity.TenantID, identity.PersonID,
		"batch_limit:remaining=12:judged=8", now.Add(3*time.Minute), now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	schedule, ok, err = store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !ok || schedule.LastOutcome != MemoryGovernanceOutcomePartial ||
		schedule.DeferredReason != "batch_limit:remaining=12:judged=8" || schedule.ConsecutiveFailure != 0 {
		t.Fatalf("partial schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
	if !schedule.LastSuccessAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("partial progress advanced complete success clock: %+v", schedule)
	}
}

func TestListPersonPartitionsPreservesTenant(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "partition-owner", "Partition Owner")
	if err != nil {
		t.Fatal(err)
	}
	partitions, err := store.ListPersonPartitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(partitions) != 1 || partitions[0].TenantID != "tenant-a" || partitions[0].PersonID != identity.PersonID {
		t.Fatalf("partitions=%+v", partitions)
	}
}

// TestMemoryGovernanceAttemptLeavesCrashRetryLease pins the crash lease. A
// process killed mid-pass records no outcome, so before this the row stayed
// overdue with last_outcome='running' and every restart re-ran the identical
// pass after the 30s startup grace, re-burning model budget with no backoff.
func TestMemoryGovernanceAttemptLeavesCrashRetryLease(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	if err := store.EnsureMemoryGovernanceSchedule(ctx, "default", "p-crash", now); err != nil {
		t.Fatal(err)
	}
	retryDue := now.Add(10 * time.Minute)
	if err := store.RecordMemoryGovernanceAttempt(ctx, "default", "p-crash", now, retryDue); err != nil {
		t.Fatal(err)
	}
	schedule, ok, err := store.MemoryGovernanceScheduleForPerson(ctx, "default", "p-crash")
	if err != nil || !ok {
		t.Fatalf("schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
	if schedule.LastOutcome != MemoryGovernanceOutcomeRunning {
		t.Errorf("outcome=%q", schedule.LastOutcome)
	}
	if !schedule.NextDueAt.Equal(retryDue) {
		t.Fatalf("next due=%s, want the crash lease %s; an unchanged due time makes the pass immediately overdue again",
			schedule.NextDueAt, retryDue)
	}
	if !schedule.NextDueAt.After(now) {
		t.Fatal("crash lease must push the due time into the future")
	}
}

// TestReconcileInterruptedMemoryGovernanceCountsFailure pins that an
// interrupted pass becomes a visible, counted failure. Without the count,
// consecutive_failures stayed at 0 forever, so neither the escalating backoff
// nor any diagnostic could see a crash loop.
func TestReconcileInterruptedMemoryGovernanceCountsFailure(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	for _, person := range []string{"p-interrupted", "p-healthy"} {
		if err := store.EnsureMemoryGovernanceSchedule(ctx, "default", person, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordMemoryGovernanceAttempt(ctx, "default", "p-interrupted", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordMemoryGovernanceSuccess(ctx, "default", "p-healthy", now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	reconcileAt := now.Add(time.Hour)
	nextDue := reconcileAt.Add(10 * time.Minute)
	count, err := store.ReconcileInterruptedMemoryGovernance(ctx, reconcileAt, nextDue)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reconciled=%d, want only the interrupted partition", count)
	}
	interrupted, _, err := store.MemoryGovernanceScheduleForPerson(ctx, "default", "p-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.LastOutcome != MemoryGovernanceOutcomeFailed {
		t.Errorf("outcome=%q, want failed", interrupted.LastOutcome)
	}
	if interrupted.ConsecutiveFailure != 1 {
		t.Errorf("consecutive failures=%d, want 1", interrupted.ConsecutiveFailure)
	}
	if interrupted.LastError == "" {
		t.Error("an interrupted pass must leave a diagnosable last_error")
	}
	if !interrupted.NextDueAt.Equal(nextDue) {
		t.Errorf("next due=%s, want %s", interrupted.NextDueAt, nextDue)
	}

	healthy, _, err := store.MemoryGovernanceScheduleForPerson(ctx, "default", "p-healthy")
	if err != nil {
		t.Fatal(err)
	}
	if healthy.LastOutcome != MemoryGovernanceOutcomeSucceeded || healthy.ConsecutiveFailure != 0 {
		t.Fatalf("a completed partition must be untouched: %+v", healthy)
	}
}
