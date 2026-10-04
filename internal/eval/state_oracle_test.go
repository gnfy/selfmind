package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }
func bp(b bool) *bool     { return &b }

func sampleWorld(t *testing.T) WorldState {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "game.html"), []byte("<html><script>tic tac toe</script></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return WorldState{
		Task:    &control.Task{ID: "t1", Status: "done", CurrentSummary: "built the game", NextSteps: []string{"polish"}, LastChannel: "cli"},
		Run:     &control.Run{ID: "r1", TaskID: "t1", Status: "done", ResumesRunID: "r0", WorkspaceID: "workspace"},
		Handoff: &control.Handoff{TaskID: "t1", Summary: "done", NextSteps: []string{"polish ui"}, ChangedFiles: []string{"game.html"}, TestStatus: "tests pass"},
		Events: []control.Event{
			{Type: "tool.completed", Payload: json.RawMessage(`{"tool":"write_file"}`)},
			{Type: "tool.completed", Payload: json.RawMessage(`{"tool":"terminal"}`)},
			{Type: "run.finished"},
		},
		Artifacts:     []control.Artifact{{TaskID: "t1", Kind: "file", Name: "game.html", URI: "game.html"}},
		Approvals:     []control.ApprovalRequest{},
		Facts:         map[string][]memory.Fact{"user": {{Target: "user", Content: "prefers vanilla JS"}}},
		WorkspaceRoot: root,
	}
}

func mustPass(t *testing.T, p StatePredicate, w WorldState) {
	t.Helper()
	if r := evalPredicate(p, w); !r.OK {
		t.Fatalf("expected pass for %+v, got: %s", p, r.Message)
	}
}
func mustFail(t *testing.T, p StatePredicate, w WorldState) {
	t.Helper()
	if r := evalPredicate(p, w); r.OK {
		t.Fatalf("expected fail for %+v, but passed", p)
	}
}

func TestStateOraclePredicates(t *testing.T) {
	w := sampleWorld(t)

	// task fields
	mustPass(t, StatePredicate{On: "task", Field: "status", Eq: sp("done")}, w)
	mustFail(t, StatePredicate{On: "task", Field: "status", Eq: sp("running")}, w)
	mustPass(t, StatePredicate{On: "task", Field: "next_steps", LenGte: ip(1)}, w)
	mustFail(t, StatePredicate{On: "task", Field: "next_steps", LenGte: ip(5)}, w)
	mustPass(t, StatePredicate{On: "task", Field: "current_summary", Contains: sp("game")}, w)
	mustPass(t, StatePredicate{On: "run", Field: "status", Eq: sp("done")}, w)
	mustPass(t, StatePredicate{On: "run", Field: "resumes_run_id", Eq: sp("r0")}, w)

	// handoff
	mustPass(t, StatePredicate{On: "handoff", Field: "changed_files", Contains: sp("game.html")}, w)
	mustPass(t, StatePredicate{On: "handoff", Field: "test_status", Contains: sp("pass")}, w)
	mustFail(t, StatePredicate{On: "handoff", Field: "next_steps", LenGte: ip(3)}, w)

	// events
	mustPass(t, StatePredicate{On: "events", Type: "tool.completed", CountGte: ip(2)}, w)
	mustFail(t, StatePredicate{On: "events", Type: "tool.completed", CountGte: ip(3)}, w)
	mustPass(t, StatePredicate{On: "events", Type: "run.cancelled", CountLte: ip(0)}, w)
	mustPass(t, StatePredicate{On: "events", Type: "tool.completed", PayloadContains: sp("write_file"), Exists: bp(true)}, w)

	// artifacts
	mustPass(t, StatePredicate{On: "artifact", Contains: sp("game.html"), CountGte: ip(1)}, w)
	mustFail(t, StatePredicate{On: "artifact", Contains: sp("nonexistent"), CountGte: ip(1)}, w)

	// file
	mustPass(t, StatePredicate{On: "file", Path: "game.html", Exists: bp(true)}, w)
	mustPass(t, StatePredicate{On: "file", Path: "game.html", Contains: sp("<script")}, w)
	mustPass(t, StatePredicate{On: "file", Path: "game.html", NotContains: sp("TODO")}, w)
	mustPass(t, StatePredicate{On: "file", Path: "game.html", MinBytes: ip(10)}, w)
	mustFail(t, StatePredicate{On: "file", Path: "game.html", MinBytes: ip(100000)}, w)
	mustFail(t, StatePredicate{On: "file", Path: "missing.txt", Exists: bp(true)}, w)
	mustPass(t, StatePredicate{On: "file", Path: "missing.txt", Exists: bp(false)}, w)

	// memory
	mustPass(t, StatePredicate{On: "memory", Target: "user", Contains: sp("vanilla JS")}, w)
	mustFail(t, StatePredicate{On: "memory", Target: "user", Contains: sp("python")}, w)

	// missing subject task
	empty := WorldState{}
	mustFail(t, StatePredicate{On: "task", Field: "status", Eq: sp("done")}, empty)
	mustFail(t, StatePredicate{On: "handoff", Field: "summary", Contains: sp("x")}, empty)
	mustFail(t, StatePredicate{On: "run", Field: "status", Eq: sp("done")}, empty)
}

func TestEvaluateStatePredicatesAggregates(t *testing.T) {
	w := sampleWorld(t)
	results := EvaluateStatePredicates([]StatePredicate{
		{On: "task", Field: "status", Eq: sp("done")},
		{On: "file", Path: "game.html", Exists: bp(true)},
	}, w)
	if len(results) != 2 || !ChecksPassed(results) {
		t.Fatalf("expected 2 passing checks, got %+v", results)
	}
}

// A live run's early milestones stay visible to the oracle however many
// events follow them. The oracle read only the newest 200, so a slower model's
// progress heartbeats pushed a committed selection out of the snapshot and the
// case reported it missing.
func TestWorldStateSeesEarlyEventsOfALongRun(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "delivery", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "deliver the receipt")
	other, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "unrelated", Channel: "cli"})
	otherRun, _ := store.StartRun(ctx, other, "cli", "unrelated work")
	appendEvent := func(run *control.Run, eventType, payload string) {
		t.Helper()
		if _, err := store.AppendEvent(ctx, control.Event{TaskID: run.TaskID, RunID: run.ID, Type: eventType, Visibility: "task", Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(run, "work.selection_committed", `{"commit_mode":"direct"}`)
	appendEvent(otherRun, "work.selection_committed", `{"commit_mode":"direct"}`)
	for i := 0; i < 1200; i++ {
		appendEvent(run, "agent.thinking", `{"message":"Receiving the model response"}`)
	}

	world := CollectWorldState(ctx, store, nil, identity, task.ID, run.ID, t.TempDir())
	committed := StatePredicate{On: "events", Type: "work.selection_committed", PayloadContains: sp(`"commit_mode":"direct"`), CountGte: ip(1), CountLte: ip(1)}
	mustPass(t, committed, world)
	mustPass(t, StatePredicate{On: "events", Type: "agent.thinking", CountGte: ip(1200)}, world)

	// A failed read is reported as the failure, not as zero events.
	store.Close()
	broken := CollectWorldState(ctx, store, nil, identity, task.ID, run.ID, t.TempDir())
	if result := evalPredicate(committed, broken); result.OK || !strings.Contains(result.Message, "read task events") {
		t.Fatalf("a failed event read gave %+v, want the read error", result)
	}
}
