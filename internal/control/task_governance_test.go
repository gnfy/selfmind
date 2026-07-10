package control

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestEnsureInboxTaskIsHiddenStableAndNeverCurrent(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.EnsureInboxTask(ctx, identity.TenantID, identity.PersonID, "workspace_1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureInboxTask(ctx, identity.TenantID, identity.PersonID, "workspace_1")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("inbox must be stable per person/workspace: %s != %s", first.ID, second.ID)
	}
	if !first.IsInbox() || first.IsVisible() || first.Status != "archived" {
		t.Fatalf("unexpected inbox shape: %+v", first)
	}
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current != nil {
		t.Fatalf("creating inbox must not change current_task: %+v", current)
	}
	if visible, err := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20); err != nil || len(visible) != 0 {
		t.Fatalf("hidden inbox leaked into ListTasks: %+v err=%v", visible, err)
	}
	if cards, err := store.ListTaskCards(ctx, identity.TenantID, identity.PersonID, 20); err != nil || len(cards) != 0 {
		t.Fatalf("hidden inbox leaked into recall cards: %+v err=%v", cards, err)
	}

	source, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "durable work"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, source, "cli", "one-off diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, source.ID, "done", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskPinned(ctx, identity.TenantID, source.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.ReassignRun(ctx, identity.TenantID, run.ID, source.ID, first.ID, false); err != nil {
		t.Fatal(err)
	}
	stats, err := store.ReadTaskGovernanceStats(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Terminal != 1 || stats.Pinned != 1 || stats.InboxRuns != 1 {
		t.Fatalf("unexpected governance stats: %+v", stats)
	}
}

func TestDeleteEmptyTaskNeverDeletesHistory(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "placeholder"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteEmptyTask(ctx, identity.TenantID, identity.PersonID, empty.ID)
	if err != nil || !deleted {
		t.Fatalf("empty placeholder deleted=%v err=%v", deleted, err)
	}
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current != nil {
		t.Fatalf("deleted placeholder left current pointer: %+v", current)
	}

	withRun, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "history"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRun(ctx, withRun, "cli", "work"); err != nil {
		t.Fatal(err)
	}
	deleted, err = store.DeleteEmptyTask(ctx, identity.TenantID, identity.PersonID, withRun.ID)
	if err != nil || deleted {
		t.Fatalf("history task deleted=%v err=%v", deleted, err)
	}
}

func TestArchiveStaleTasksHonorsPinnedPendingAndOpenWork(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	create := func(title, status string) *Task {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, "", nil); err != nil {
			t.Fatal(err)
		}
		return task
	}

	staleDone := create("stale done", "done")
	pinned := create("pinned done", "done")
	if err := store.SetTaskPinned(ctx, identity.TenantID, pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	pending := create("cancelled with approval", "cancelled")
	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: pending.ID,
	}); err != nil {
		t.Fatal(err)
	}
	open := create("still open", "interrupted")
	unpinned := create("old work with recent governance change", "done")
	old := time.Now().Add(-45 * 24 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET updated_at = ?, last_activity_at = ?`, old, old); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskPinned(ctx, identity.TenantID, unpinned.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskPinned(ctx, identity.TenantID, unpinned.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, staleDone.ID); err != nil {
		t.Fatal(err)
	}

	archived, err := store.ArchiveStaleTasks(ctx, time.Now(), 30*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	archivedIDs := map[string]bool{}
	for _, task := range archived {
		archivedIDs[task.TaskID] = true
	}
	if len(archived) != 2 || !archivedIDs[staleDone.ID] || !archivedIDs[unpinned.ID] {
		t.Fatalf("archived=%+v, want stale done and recently unpinned old work", archived)
	}
	assertStatus := func(task *Task, want string) {
		t.Helper()
		got, err := store.GetTask(ctx, identity.TenantID, task.ID)
		if err != nil || got == nil || got.Status != want {
			t.Fatalf("task %s status=%+v err=%v, want %s", task.Title, got, err, want)
		}
	}
	assertStatus(staleDone, "archived")
	assertStatus(pinned, "done")
	assertStatus(pending, "cancelled")
	assertStatus(open, "interrupted")
	assertStatus(unpinned, "archived")
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current != nil {
		t.Fatalf("auto-archived current pointer must be cleared: %+v", current)
	}
}

func TestSearchTasksFindsOlderCJKRunAndHandoff(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}

	target, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "九七对战游戏",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, target, "cli", "继续优化跳攻和打击反馈")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, Handoff{
		TaskID: target.ID, Summary: "air attack complete", ChangedFiles: []string{"game/arcade-fury-97.html"},
	}); err != nil {
		t.Fatal(err)
	}

	// Push target outside the legacy /tasks 100-row window. Search must still
	// query the complete visible history rather than filtering that window.
	for i := 0; i < 101; i++ {
		if _, err := store.CreateTask(ctx, TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: fmt.Sprintf("noise %03d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if recent, err := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 100); err != nil {
		t.Fatal(err)
	} else {
		for _, task := range recent {
			if task.ID == target.ID {
				t.Fatal("test setup failed: target is still inside the recent 100-row window")
			}
		}
	}

	for _, query := range []string{"跳攻", "arcade-fury"} {
		matches, err := store.SearchTasks(ctx, identity.TenantID, identity.PersonID, query, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 1 || matches[0].ID != target.ID {
			t.Fatalf("query %q matches=%+v, want target %s", query, matches, target.ID)
		}
	}
	inbox, err := store.EnsureInboxTask(ctx, identity.TenantID, identity.PersonID, "")
	if err != nil || inbox == nil {
		t.Fatalf("EnsureInboxTask: inbox=%+v err=%v", inbox, err)
	}
	if matches, err := store.SearchTasks(ctx, identity.TenantID, identity.PersonID, "Inbox", 20); err != nil || len(matches) != 0 {
		t.Fatalf("hidden inbox leaked into search: matches=%+v err=%v", matches, err)
	}
}

func TestQueryTasksFiltersWorkspaceStatusKeywordAndPaginates(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	create := func(title, workspace, status string) *Task {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			WorkspaceID: workspace, Title: title,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, title+" summary", nil); err != nil {
			t.Fatal(err)
		}
		return task
	}
	create("alpha API", "workspace_a", "in_progress")
	create("alpha CLI", "workspace_a", "interrupted")
	create("alpha done", "workspace_a", "done")
	create("beta API", "workspace_b", "in_progress")

	page, err := store.QueryTasks(ctx, identity.TenantID, identity.PersonID, TaskQuery{
		View: "open", WorkspaceID: "workspace_a", Keyword: "alpha", Limit: 1, Offset: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Tasks) != 1 || !page.HasMore() {
		t.Fatalf("page=%+v", page)
	}
	done, err := store.QueryTasks(ctx, identity.TenantID, identity.PersonID, TaskQuery{
		View: "all", Status: "done", WorkspaceID: "workspace_a", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Total != 1 || len(done.Tasks) != 1 || done.Tasks[0].Title != "alpha done" {
		t.Fatalf("done page=%+v", done)
	}
}
