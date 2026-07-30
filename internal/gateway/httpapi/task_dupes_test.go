package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func newDupeHarness(t *testing.T) (*Server, *control.IdentityContext) {
	t.Helper()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "tenant", "cli", "local", "Tester")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, DefaultTenantID: identity.TenantID}, identity
}

func dupeTask(t *testing.T, d *Server, identity *control.IdentityContext, title, workspaceID string) *control.Task {
	t.Helper()
	task, err := d.Control.CreateTask(context.Background(), control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestSuggestDuplicateTasks: near-identical open titles in the SAME workspace
// get exactly one suggestion, idempotent across sweeps; cross-workspace
// twins and unrelated titles never pair.
func TestSuggestDuplicateTasks(t *testing.T) {
	d, identity := newDupeHarness(t)
	ctx := context.Background()
	a := dupeTask(t, d, identity, "拳皇97风格格斗游戏 优化打击感", "ws1")
	b := dupeTask(t, d, identity, "拳皇97风格格斗游戏 优化打击感和跳跃", "ws1")
	dupeTask(t, d, identity, "拳皇97风格格斗游戏 优化打击感", "ws2") // other workspace
	dupeTask(t, d, identity, "周报数据统计脚本", "ws1")         // unrelated

	if got := d.suggestDuplicateTasks(ctx); got != 1 {
		t.Fatalf("expected exactly 1 suggestion, got %d", got)
	}
	// Idempotent: the second sweep records nothing new.
	if got := d.suggestDuplicateTasks(ctx); got != 0 {
		t.Fatalf("second sweep must be idempotent, got %d", got)
	}
	suggestions, err := d.Control.ListDuplicateSuggestions(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("expected one recorded pair: %+v", suggestions)
	}
	for newer, older := range suggestions {
		pair := map[string]bool{newer: true, older: true}
		if !pair[a.ID] || !pair[b.ID] {
			t.Fatalf("pair must be a+b: %s -> %s", newer, older)
		}
	}
}

// TestTaskMergeCommand: /task <src> merge <dst> moves history, archives the
// source, and the duplicate line disappears from /tasks.
func TestTaskMergeCommand(t *testing.T) {
	d, identity := newDupeHarness(t)
	ctx := context.Background()
	src := dupeTask(t, d, identity, "tank game duplicate", "ws1")
	dst := dupeTask(t, d, identity, "tank game", "ws1")
	run, err := d.Control.StartRun(ctx, src, "cli", "build")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Control.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}

	reply, err := d.taskCommandReply(ctx, identity, []string{src.ID, "merge", dst.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "1 run(s) moved") {
		t.Fatalf("merge reply must report moved runs: %s", reply)
	}
	srcAfter, err := d.Control.GetTask(ctx, identity.TenantID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if srcAfter.Status != "archived" {
		t.Fatalf("source must be archived: %s", srcAfter.Status)
	}
	events, _ := d.Control.ListTaskEvents(ctx, dst.ID, 20)
	var sawMerged bool
	for _, e := range events {
		if e.Type == "task.merged" {
			sawMerged = true
		}
	}
	if !sawMerged {
		t.Fatal("task.merged event must be recorded on dst")
	}

	// Self-merge and missing-arg guards stay friendly.
	if reply, _ := d.taskCommandReply(ctx, identity, []string{dst.ID, "merge", dst.ID}); !strings.Contains(reply, "same task") {
		t.Fatalf("self-merge must be refused: %s", reply)
	}
	if reply, _ := d.taskCommandReply(ctx, identity, []string{dst.ID, "merge"}); !strings.Contains(reply, "Usage:") {
		t.Fatalf("missing dst must show usage: %s", reply)
	}
}

// TestDupeSuggestionsForView: only pairs with BOTH sides still visible render.
func TestDupeSuggestionsForView(t *testing.T) {
	tasks := []control.Task{{ID: "task_a"}, {ID: "task_b"}}
	got := dupeSuggestionsForView(map[string]string{
		"task_a": "task_b", // both visible
		"task_c": "task_a", // c not visible
		"task_b": "task_d", // d not visible
	}, tasks)
	if len(got) != 1 || got["task_a"] != "task_b" {
		t.Fatalf("only fully visible pairs may render: %+v", got)
	}
}
