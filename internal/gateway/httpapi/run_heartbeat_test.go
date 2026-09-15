package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

// The heartbeat goroutine used to read run.TenantID and run.ID through the
// *control.Run it was handed, while refreshDirectContinuation rewrote that
// struct in place (*run = *fresh) mid-turn — a data race the race detector
// caught in CI on 2026-09-15 (work_select moving a large_read continuation onto
// its parent thread). The heartbeat must hold the run's identity as values: a
// run's tenant and id never change, but the struct behind the pointer does.
//
// The stop closure shared the same read, and it is deterministic to observe:
// rewrite the struct, stop, and sweep. A heartbeat addressed to the rewritten
// (empty) id updates nothing, so the sweeper declares the real run dead.
func TestRunHeartbeatSurvivesInPlaceRewriteOfTheRun(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "long turn")
	if err != nil {
		t.Fatal(err)
	}
	coord := (&Server{Control: store, DefaultTenantID: "default"}).coordinator()

	stop := coord.startRunHeartbeat(ctx, run, "", "")
	// heartbeat_at has whole-second resolution and the sweeper's comparison is
	// inclusive, so the original row, the stop-time heartbeat, and the sweep
	// cutoff must each land in distinct seconds: start … 3.1s … stop, then a
	// 2s threshold puts the cutoff strictly after started_at and strictly
	// before the fresh heartbeat.
	time.Sleep(3100 * time.Millisecond)
	// What refreshDirectContinuation does mid-turn: the struct behind the
	// pointer is replaced wholesale.
	*run = control.Run{}
	stop()

	// Only a heartbeat that reached the REAL run keeps it alive; one addressed
	// to the rewritten (empty) id updates nothing, and the row's started_at is
	// then older than the threshold.
	interrupted, err := store.MarkInterruptedRuns(ctx, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted != 0 {
		t.Fatalf("the heartbeat lost its run after the struct was rewritten: sweeper interrupted %d run(s)", interrupted)
	}
}
