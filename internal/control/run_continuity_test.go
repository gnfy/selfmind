package control

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type testSQLiteError struct {
	code int
}

func (e testSQLiteError) Error() string { return fmt.Sprintf("sqlite error (%d)", e.code) }
func (e testSQLiteError) Code() int     { return e.code }

func TestIsSQLiteBusyRecognizesExtendedCodes(t *testing.T) {
	for _, code := range []int{5, 261, 517, 773} {
		err := fmt.Errorf("wrapped: %w", testSQLiteError{code: code})
		if !isSQLiteBusy(err) {
			t.Errorf("code %d was not recognized as SQLITE_BUSY", code)
		}
	}
	if isSQLiteBusy(testSQLiteError{code: 6}) {
		t.Fatal("SQLITE_LOCKED was misclassified as SQLITE_BUSY")
	}
}

func continuityFixture(t *testing.T) (*Store, *IdentityContext, *Task) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continuity", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, identity, task
}

func TestListUnresolvedRunsFiltersClaimedAndTerminal(t *testing.T) {
	store, identity, task := continuityFixture(t)
	ctx := context.Background()
	mk := func(input, status string) *Run {
		run, err := store.StartRun(ctx, task, "cli", input)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
			t.Fatal(err)
		}
		return run
	}
	waiting := mk("waiting work", "waiting_user")
	mk("done work", "done")
	claimed := mk("claimed work", "interrupted")
	child, err := store.StartRunWithOptions(ctx, task, "cli", "the claiming child", StartRunOptions{ParentRunID: claimed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentRunID != claimed.ID {
		t.Fatalf("child parent edge = %q, want %s", child.ParentRunID, claimed.ID)
	}

	runs, err := store.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != waiting.ID {
		t.Fatalf("unresolved runs = %+v, want exactly the waiting run %s", runs, waiting.ID)
	}
}

func TestAtomicParentClaimSingleWinnerAndLegacyBlockerHygiene(t *testing.T) {
	store, identity, task := continuityFixture(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "interruptible work")
	if err != nil {
		t.Fatal(err)
	}
	// MaterializeRunFinalization is the production terminal path. Since §10.3
	// it creates NO blocker row: the unclaimed interrupted run itself is the
	// wait authority, so the task projection must derive "interrupted".
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: parent.ID, RunStatus: "interrupted",
		TaskID: task.ID, TaskStatus: "interrupted", Summary: "crashed midway",
	}); err != nil {
		t.Fatal(err)
	}
	openLegacyBlockers := func() int {
		t.Helper()
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_blockers
			WHERE tenant_id = ? AND thread_id = ? AND status = 'open'`,
			identity.TenantID, task.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if count := openLegacyBlockers(); count != 0 {
		t.Fatalf("finalization must not create blocker rows anymore, found %d", count)
	}
	// Seed one open row the way a pre-simplification daemon left it; the claim
	// must settle it (legacy hygiene via resolveOriginRunBlockersTx).
	if _, err := store.db.ExecContext(ctx, `INSERT INTO task_blockers
		(id, tenant_id, person_id, thread_id, origin_run_id, kind, status, detail_json, created_at)
		VALUES ('blocker_legacy', ?, ?, ?, ?, 'run_interrupted', 'open', '{}', 1)`,
		identity.TenantID, identity.PersonID, task.ID, parent.ID); err != nil {
		t.Fatal(err)
	}

	childA, err := store.StartRunWithOptions(ctx, task, "cli", "first continuation", StartRunOptions{ParentRunID: parent.ID})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if childA.ParentRunID != parent.ID {
		t.Fatalf("first child parent edge = %q", childA.ParentRunID)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "weixin", "second continuation", StartRunOptions{ParentRunID: parent.ID}); !errors.Is(err, ErrParentRunClaimed) {
		t.Fatalf("second claim must lose with ErrParentRunClaimed, got %v", err)
	}
	if count := openLegacyBlockers(); count != 0 {
		t.Fatalf("winning claim must settle legacy blocker rows, %d still open", count)
	}
	// A terminal parent can never be claimed.
	done, err := store.StartRun(ctx, task, "cli", "already finished")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, done.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "late continuation", StartRunOptions{ParentRunID: done.ID}); !errors.Is(err, ErrParentRunNotResumable) {
		t.Fatalf("terminal parent must refuse the claim, got %v", err)
	}
	// A failed claim creates nothing: no orphan child rows.
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("lost claims must not create runs, got %d rows", len(runs))
	}
}

// TestConcurrentParentClaimAcrossConnections proves the unique partial index
// is a real cross-connection guard: two INDEPENDENT store connections racing
// to claim one parent produce exactly one child (the P1 acceptance criterion —
// the single-connection serialization of one Store proves nothing).
func TestConcurrentParentClaimAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	storeA, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	ctx := context.Background()
	identity, err := storeA.ResolveOrCreateAccount(ctx, "default", "cli", "local", "User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := storeA.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "race", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := storeA.StartRun(ctx, task, "cli", "parked work")
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	storeB, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	type result struct {
		run *Run
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, store := range []*Store{storeA, storeB} {
		go func(s *Store) {
			<-start
			run, err := s.StartRunWithOptions(ctx, task, "cli", "continuation", StartRunOptions{ParentRunID: parent.ID})
			results <- result{run: run, err: err}
		}(store)
	}
	close(start)
	var wins, losses int
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err == nil && r.run != nil && r.run.ParentRunID == parent.ID:
			wins++
		case errors.Is(r.err, ErrParentRunClaimed):
			losses++
		default:
			t.Fatalf("unexpected race outcome: run=%+v err=%v", r.run, r.err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d, want exactly one child and one claimed result", wins, losses)
	}
	var children int
	if err := storeA.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND parent_run_id = ?`,
		identity.TenantID, parent.ID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 1 {
		t.Fatalf("children=%d, want exactly 1 (no fork)", children)
	}
}

func TestRunHandoffReadsFinalizationKey(t *testing.T) {
	store, identity, task := continuityFixture(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "with handoff")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, Handoff{
		ID: "handoff_run_" + run.ID, TaskID: task.ID, Summary: "run-scoped summary",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.RunHandoff(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || got == nil || got.Summary != "run-scoped summary" {
		t.Fatalf("RunHandoff = %+v err=%v", got, err)
	}
	missing, err := store.RunHandoff(ctx, identity.TenantID, identity.PersonID, "run_never-finalized")
	if err != nil || missing != nil {
		t.Fatalf("missing handoff must be nil, got %+v err=%v", missing, err)
	}
	_ = identity
}

func TestIncompleteLoopCheckpointForRunIsRunExact(t *testing.T) {
	store, identity, task := continuityFixture(t)
	ctx := context.Background()
	runA, err := store.StartRun(ctx, task, "cli", "line A")
	if err != nil {
		t.Fatal(err)
	}
	runB, err := store.StartRun(ctx, task, "cli", "line B")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveLoopCheckpoint(ctx, LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: runA.ID,
		Iteration: 1, Outcome: "continue", Snapshot: []byte(`[]`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveLoopCheckpoint(ctx, LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: runB.ID,
		Iteration: 9, Outcome: "complete_turn", Snapshot: []byte(`[]`),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, runA.ID)
	if err != nil || got == nil || got.RunID != runA.ID {
		t.Fatalf("checkpoint for run A = %+v err=%v", got, err)
	}
	complete, err := store.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, runB.ID)
	if err != nil || complete != nil {
		t.Fatalf("a complete_turn checkpoint must not restore: %+v err=%v", complete, err)
	}
}
