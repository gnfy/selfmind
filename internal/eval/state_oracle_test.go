package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		Run:     &control.Run{ID: "r1", TaskID: "t1", Status: "done", ParentRunID: "r0", WorkspaceID: "workspace"},
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
	mustPass(t, StatePredicate{On: "run", Field: "parent_run_id", Eq: sp("r0")}, w)

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
