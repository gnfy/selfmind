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
	store := newTestStore(t)
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
	child, err := store.StartRunWithOptions(ctx, task, "cli", "the claiming child", StartRunOptions{ResumesRunID: claimed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if child.ResumesRunID != claimed.ID {
		t.Fatalf("child parent edge = %q, want %s", child.ResumesRunID, claimed.ID)
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

	childA, err := store.StartRunWithOptions(ctx, task, "cli", "first continuation", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if childA.ResumesRunID != parent.ID {
		t.Fatalf("first child parent edge = %q", childA.ResumesRunID)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "weixin", "second continuation", StartRunOptions{ResumesRunID: parent.ID}); !errors.Is(err, ErrResumeTargetClaimed) {
		t.Fatalf("second claim must lose with ErrResumeTargetClaimed, got %v", err)
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
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "late continuation", StartRunOptions{ResumesRunID: done.ID}); !errors.Is(err, ErrResumeTargetNotResumable) {
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
			run, err := s.StartRunWithOptions(ctx, task, "cli", "continuation", StartRunOptions{ResumesRunID: parent.ID})
			results <- result{run: run, err: err}
		}(store)
	}
	close(start)
	var wins, losses int
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err == nil && r.run != nil && r.run.ResumesRunID == parent.ID:
			wins++
		case errors.Is(r.err, ErrResumeTargetClaimed):
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
		`SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND resumes_run_id = ?`,
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

// TestResumeChainWalksOnlyRecordedEdges pins the read-time grouping. The chain
// is the only evidence that several runs are one line of work, and it exists
// only where something was actually continued — so a standalone run is its own
// root rather than being folded into a neighbour.
func TestResumeChainWalksOnlyRecordedEdges(t *testing.T) {
	ctx := context.Background()
	store, identity, task, root := newRecoveryFixture(t)

	if err := store.FinishRun(ctx, identity.TenantID, root.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	second, err := store.StartRunWithOptions(ctx, task, "cli", "continued once", StartRunOptions{ResumesRunID: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, second.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	third, err := store.StartRunWithOptions(ctx, task, "cli", "continued twice", StartRunOptions{ResumesRunID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := store.StartRun(ctx, task, "cli", "unrelated work")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, runID, wantRoot string }{
		{"deepest", third.ID, root.ID},
		{"middle", second.ID, root.ID},
		{"root itself", root.ID, root.ID},
		{"standalone run is its own root", standalone.ID, standalone.ID},
		{"unknown run resolves to itself", "run_missing", "run_missing"},
		{"empty", "", ""},
	} {
		got, err := store.ResumeChainRoot(ctx, identity.TenantID, tc.runID)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.wantRoot {
			t.Fatalf("%s: root=%q want %q", tc.name, got, tc.wantRoot)
		}
	}

	chain, err := store.ResumeChainRunIDs(ctx, identity.TenantID, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{third.ID, second.ID, root.ID}
	if len(chain) != len(want) {
		t.Fatalf("chain=%v want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain[%d]=%q want %q (full %v)", i, chain[i], want[i], chain)
		}
	}
	if solo, err := store.ResumeChainRunIDs(ctx, identity.TenantID, standalone.ID); err != nil || len(solo) != 1 || solo[0] != standalone.ID {
		t.Fatalf("standalone chain=%v err=%v", solo, err)
	}
}

// TestSimultaneousResumeClaimReturnsTheSentinel pins the race fallback. The
// unique index is the last line of defence when two commits validate on
// snapshots that cannot see each other, and the caller must be able to tell
// "someone else claimed it" from a storage failure. Matching the index NAME
// never worked: SQLite names the columns.
func TestSimultaneousResumeClaimReturnsTheSentinel(t *testing.T) {
	ctx := context.Background()
	store, identity, task, root := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, root.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "winner", StartRunOptions{ResumesRunID: root.ID}); err != nil {
		t.Fatal(err)
	}
	// Reproduce exactly what the loser's INSERT hits, without the validation
	// that would normally short-circuit it.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO runs (id, thread_id, tenant_id, person_id, channel, input_summary, status, started_at, resumes_run_id)
		 VALUES ('run_race_loser', ?, ?, ?, 'cli', 'loser', 'running', 99, ?)`,
		task.ID, identity.TenantID, identity.PersonID, root.ID)
	if err == nil {
		t.Fatal("the partial unique index did not fire")
	}
	if !isResumeEdgeUniqueViolation(err) {
		t.Fatalf("a lost race must be recognizable as a claim conflict, got %v", err)
	}
	// An unrelated failure must not be mistaken for one.
	if isResumeEdgeUniqueViolation(errors.New("disk I/O error")) {
		t.Fatal("an unrelated error was reported as a claim conflict")
	}
}

// TestThreadlessRunIsCompleteWork proves the capability Batch 3 exists to
// enable: a Run with no Thread is a full unit of work. It executes, parks,
// stays Attention, resolves by ordinal-free exact id, and can be resumed —
// none of which ever needed a label. Requiring one meant every message minted
// a container before anyone knew what the work was.
func TestThreadlessRunIsCompleteWork(t *testing.T) {
	ctx := context.Background()
	store, identity, _, _ := newRecoveryFixture(t)
	owner := RunOwner{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "ws_1"}

	run, err := store.StartRunForOwner(ctx, owner, "cli", "work with no label", StartRunOptions{})
	if err != nil {
		t.Fatalf("a run must not require a thread: %v", err)
	}
	if run.TaskID != "" {
		t.Fatalf("thread id = %q, want empty", run.TaskID)
	}
	if run.WorkspaceID != "ws_1" {
		t.Fatalf("workspace = %q, want ws_1", run.WorkspaceID)
	}

	// It parks like any other run.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	fetched, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || fetched == nil || fetched.Status != "waiting_user" {
		t.Fatalf("run=%+v err=%v", fetched, err)
	}

	// It is Attention, with the run's own summary standing in for the missing
	// label — which is what a person recognizes it by anyway.
	items, err := NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var found *AttentionItem
	for i := range items {
		if items[i].RunID == run.ID {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatalf("a thread-less run vanished from Attention: %+v", items)
	}
	if found.Thread.ID != "" {
		t.Fatalf("attention invented a thread: %q", found.Thread.ID)
	}
	if found.Thread.Title != "work with no label" {
		t.Fatalf("attention title = %q, want the run summary", found.Thread.Title)
	}
	if found.Thread.TenantID != identity.TenantID || found.Thread.PersonID != identity.PersonID {
		t.Fatalf("identity must come from the run: %+v", found.Thread)
	}

	// It is listed as resumable and can actually be resumed.
	unresolved, err := store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, candidate := range unresolved {
		if candidate.ID == run.ID {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("a thread-less run vanished from the resumable listing: %+v", unresolved)
	}
	resumer, err := store.StartRunForOwner(ctx, owner, "cli", "continue it", StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatalf("a thread-less run must be resumable: %v", err)
	}
	if resumer.ResumesRunID != run.ID {
		t.Fatalf("resume edge = %q, want %s", resumer.ResumesRunID, run.ID)
	}
	root, err := store.ResumeChainRoot(ctx, identity.TenantID, resumer.ID)
	if err != nil || root != run.ID {
		t.Fatalf("chain root=%q err=%v, want %s", root, err, run.ID)
	}

	// A run owner with no person is refused: identity is the one thing a Run
	// genuinely cannot do without.
	if _, err := store.StartRunForOwner(ctx, RunOwner{TenantID: identity.TenantID}, "cli", "no owner", StartRunOptions{}); err == nil {
		t.Fatal("a run without a person must be refused")
	}
}

// TestResumeLineCoversTheWholeChainFromAnyMember pins the replacement for "the
// runs of this Task": a line of work is the connected resume chain, reachable
// from any member, and a standalone run is a line of one.
func TestResumeLineCoversTheWholeChainFromAnyMember(t *testing.T) {
	ctx := context.Background()
	store, identity, task, root := newRecoveryFixture(t)
	build := func(prev *Run, input string) *Run {
		t.Helper()
		if err := store.FinishRun(ctx, identity.TenantID, prev.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		next, err := store.StartRunWithOptions(ctx, task, "cli", input, StartRunOptions{ResumesRunID: prev.ID})
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	second := build(root, "second")
	third := build(second, "third")
	standalone, err := store.StartRun(ctx, task, "cli", "unrelated")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{root.ID, second.ID, third.ID}
	for _, from := range want {
		line, err := store.ResumeLineRunIDs(ctx, identity.TenantID, from)
		if err != nil {
			t.Fatalf("from %s: %v", from, err)
		}
		if len(line) != len(want) {
			t.Fatalf("from %s: line=%v want %v", from, line, want)
		}
		for i := range want {
			if line[i] != want[i] {
				t.Fatalf("from %s: line=%v want oldest-first %v", from, line, want)
			}
		}
	}

	solo, err := store.ResumeLineRunIDs(ctx, identity.TenantID, standalone.ID)
	if err != nil || len(solo) != 1 || solo[0] != standalone.ID {
		t.Fatalf("a standalone run must be a line of one: %v err=%v", solo, err)
	}
	for _, id := range solo {
		if id == root.ID {
			t.Fatal("an unrelated run was swept into the line")
		}
	}
	if empty, err := store.ResumeLineRunIDs(ctx, identity.TenantID, ""); err != nil || empty != nil {
		t.Fatalf("empty run id: %v err=%v", empty, err)
	}
}
