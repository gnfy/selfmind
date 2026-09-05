package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/modelchange"
)

type testMemoryConsolidator struct {
	calls  []string
	err    error
	result *MemoryGovernanceResult
	every  time.Duration
	pause  bool
}

func TestMemoryGovernancePassStopsWhenAReadyModelBecomesPending(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	if _, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "owner", "Owner"); err != nil {
		t.Fatal(err)
	}
	service, _ := testModelChangeService(t)
	if _, err := service.AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Auxiliary.Model = "background-pending"
	if _, err := service.Prepare(ctx, modelchange.PrepareRequest{
		Candidate: candidate, Source: "test", RequireConfirmation: true,
	}); err != nil {
		t.Fatal(err)
	}
	consolidator := &testMemoryConsolidator{every: 24 * time.Hour}
	server := &Server{Control: store, ModelChanges: service, MemoryConsolidator: consolidator}
	if delay := server.runMemoryGovernancePassAt(ctx, time.Now()); delay <= 0 {
		t.Fatalf("readiness retry delay = %s", delay)
	}
	if len(consolidator.calls) != 0 {
		t.Fatalf("memory governance calls = %v after Model Readiness became pending", consolidator.calls)
	}
}

func (c *testMemoryConsolidator) RunGovernanceOnce(_ context.Context, personID string) (MemoryGovernanceResult, error) {
	c.calls = append(c.calls, personID)
	if c.result != nil {
		return *c.result, c.err
	}
	return MemoryGovernanceResult{Complete: true, StopReason: "complete"}, c.err
}

func (c *testMemoryConsolidator) Interval() time.Duration   { return c.every }
func (c *testMemoryConsolidator) PauseWhileRunActive() bool { return c.pause }
func (c *testMemoryConsolidator) Mode() string              { return "shadow" }

func TestMemoryGovernanceScheduleSurvivesRestartClock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "governance-owner", "Governance Owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	first := &testMemoryConsolidator{every: 24 * time.Hour}
	server := &Server{Control: store, MemoryConsolidator: first}
	if delay := server.runMemoryGovernancePassAt(ctx, now); delay < 23*time.Hour {
		t.Fatalf("next delay=%s", delay)
	}
	if len(first.calls) != 1 || first.calls[0] != identity.PersonID {
		t.Fatalf("calls=%v", first.calls)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second := &testMemoryConsolidator{every: 24 * time.Hour}
	restarted := &Server{Control: reopened, MemoryConsolidator: second}
	if delay := restarted.runMemoryGovernancePassAt(ctx, now.Add(time.Hour)); delay < 22*time.Hour {
		t.Fatalf("restart delay=%s", delay)
	}
	if len(second.calls) != 0 {
		t.Fatalf("future schedule reran after restart: %v", second.calls)
	}
}

func TestMemoryGovernanceFailureRetriesWithoutFullInterval(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "retry-owner", "Retry Owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	consolidator := &testMemoryConsolidator{every: 24 * time.Hour, err: errors.New("temporary provider failure")}
	server := &Server{Control: store, MemoryConsolidator: consolidator}
	if delay := server.runMemoryGovernancePassAt(ctx, now); delay > memoryGovernanceRetryDelay+time.Second {
		t.Fatalf("failure retry delay=%s", delay)
	}
	schedule, ok, err := store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !ok {
		t.Fatalf("schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
	if schedule.LastOutcome != control.MemoryGovernanceOutcomeFailed || schedule.ConsecutiveFailure != 1 {
		t.Fatalf("failure schedule=%+v", schedule)
	}
}

func TestMemoryGovernanceActiveRunDefersDueWork(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "active-owner", "Active Owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	consolidator := &testMemoryConsolidator{every: 24 * time.Hour, pause: true}
	server := &Server{Control: store, MemoryConsolidator: consolidator}
	server.coordinator().beginActive(identity.PersonID, &activeRun{PersonID: identity.PersonID, RunID: "run_active"})
	defer server.coordinator().endActive(identity.PersonID)
	if delay := server.runMemoryGovernancePassAt(ctx, now); delay != memoryGovernanceRetryDelay {
		t.Fatalf("active retry delay=%s", delay)
	}
	if len(consolidator.calls) != 0 {
		t.Fatalf("active run did not pause governance: %v", consolidator.calls)
	}
	schedule, ok, err := store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !ok || schedule.LastOutcome != control.MemoryGovernanceOutcomeDeferred || schedule.DeferredReason != "foreground_run_active" {
		t.Fatalf("deferred schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
}

func TestMemoryGovernancePartialProgressSchedulesBoundedCatchUp(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "partial-owner", "Partial Owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	result := MemoryGovernanceResult{CandidateGroups: 165, Judged: 6, Remaining: 159, Complete: false, StopReason: "deadline"}
	consolidator := &testMemoryConsolidator{every: 24 * time.Hour, result: &result}
	server := &Server{Control: store, MemoryConsolidator: consolidator}
	delay := server.runMemoryGovernancePassAt(ctx, now)
	if delay < memoryGovernanceBacklogRetryDelay-time.Second || delay > memoryGovernanceBacklogRetryDelay+time.Second {
		t.Fatalf("partial retry delay=%s", delay)
	}
	schedule, ok, err := store.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !ok {
		t.Fatalf("schedule=%+v ok=%v err=%v", schedule, ok, err)
	}
	if schedule.LastOutcome != control.MemoryGovernanceOutcomePartial || schedule.ConsecutiveFailure != 0 ||
		!strings.Contains(schedule.DeferredReason, "deadline:remaining=159:judged=6") || !schedule.LastSuccessAt.IsZero() {
		t.Fatalf("partial schedule=%+v", schedule)
	}
}

// TestMemoryGovernanceBackoffEscalatesAndCaps pins the retry clock. A crash
// loop previously retried at the base delay forever because the durable failure
// counter never advanced; the counter only became meaningful once interrupted
// passes were reconciled into failures.
func TestMemoryGovernanceBackoffEscalatesAndCaps(t *testing.T) {
	if got := memoryGovernanceBackoff(0); got != memoryGovernanceRetryDelay {
		t.Errorf("backoff(0)=%s, want the base delay %s", got, memoryGovernanceRetryDelay)
	}
	if got := memoryGovernanceBackoff(1); got != memoryGovernanceRetryDelay {
		t.Errorf("backoff(1)=%s, want the base delay %s", got, memoryGovernanceRetryDelay)
	}
	if got := memoryGovernanceBackoff(2); got != 2*memoryGovernanceRetryDelay {
		t.Errorf("backoff(2)=%s, want %s", got, 2*memoryGovernanceRetryDelay)
	}
	if got := memoryGovernanceBackoff(3); got != 4*memoryGovernanceRetryDelay {
		t.Errorf("backoff(3)=%s, want %s", got, 4*memoryGovernanceRetryDelay)
	}
	previous := time.Duration(0)
	for failures := 1; failures <= 40; failures++ {
		got := memoryGovernanceBackoff(failures)
		if got > memoryGovernanceMaxRetryDelay {
			t.Fatalf("backoff(%d)=%s exceeds the cap %s", failures, got, memoryGovernanceMaxRetryDelay)
		}
		if got < previous {
			t.Fatalf("backoff(%d)=%s went backwards from %s", failures, got, previous)
		}
		previous = got
	}
	if previous != memoryGovernanceMaxRetryDelay {
		t.Errorf("backoff should reach the cap %s, got %s", memoryGovernanceMaxRetryDelay, previous)
	}
}
