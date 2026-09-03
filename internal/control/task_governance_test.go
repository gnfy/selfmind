package control

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

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

	withReference, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "named placeholder"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: withReference.ID,
		Class: TaskReferenceLiteral, Value: "named-placeholder", UserConfirmed: true,
		Provenance: "user_control", SourceRef: "test",
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err = store.DeleteEmptyTask(ctx, identity.TenantID, identity.PersonID, withReference.ID)
	if err != nil || deleted {
		t.Fatalf("user-governed reference must make a label durable: deleted=%v err=%v", deleted, err)
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
	create := func(title, status string) (*Task, *Run) {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
			t.Fatal(err)
		}
		return task, run
	}

	staleDone, _ := create("stale done", "done")
	pinned, _ := create("pinned done", "done")
	if err := store.SetTaskPinned(ctx, identity.TenantID, pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	pending, pendingRun := create("cancelled with approval", "cancelled")
	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: pending.ID, RunID: pendingRun.ID,
	}); err != nil {
		t.Fatal(err)
	}
	open, _ := create("still open", "interrupted")
	unpinned, _ := create("old work with recent governance change", "done")
	old := time.Now().Add(-45 * 24 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx, `UPDATE threads SET updated_at = ?, last_activity_at = ?`, old, old); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskPinned(ctx, identity.TenantID, unpinned.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskPinned(ctx, identity.TenantID, unpinned.ID, false); err != nil {
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
	assertVisibility := func(task *Task, want string) {
		t.Helper()
		got, err := store.GetTask(ctx, identity.TenantID, task.ID)
		if err != nil || got == nil || got.Visibility != want {
			t.Fatalf("thread %s projection=%+v err=%v, want visibility %s", task.Title, got, err, want)
		}
	}
	assertVisibility(staleDone, ThreadVisibilityArchived)
	assertVisibility(pinned, ThreadVisibilityListed)
	assertVisibility(pending, ThreadVisibilityListed)
	assertVisibility(open, ThreadVisibilityListed)
	assertVisibility(unpinned, ThreadVisibilityArchived)
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

	// Search must query complete retained history rather than a recent-card
	// window, even after substantial newer noise.
	for i := 0; i < 101; i++ {
		if _, err := store.CreateTask(ctx, TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: fmt.Sprintf("noise %03d", i),
		}); err != nil {
			t.Fatal(err)
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
		run, err := store.StartRun(ctx, task, "cli", title+" summary")
		if err != nil {
			t.Fatal(err)
		}
		if status != "in_progress" {
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
				t.Fatal(err)
			}
		}
		return task
	}
	create("alpha API", "workspace_a", "in_progress")
	// A real park: an interrupted run without work evidence is no longer
	// Attention, a waiting_user run always is.
	create("alpha CLI", "workspace_a", "waiting_user")
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

// TestGetTaskStatusFollowsAttentionRules pins the compatibility Task.status
// projection to the one Attention judge: an evidence-free interrupted run and
// a dismissed run read as settled ('done'), an interrupted run with a plan
// stays interrupted, and a pending approval reaches the Thread only through
// its undismissed Run.
func TestGetTaskStatusFollowsAttentionRules(t *testing.T) {
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
	park := func(title, status string) (*Task, *Run) {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		return task, run
	}
	status := func(taskID string) string {
		t.Helper()
		got, err := store.GetTask(ctx, identity.TenantID, taskID)
		if err != nil || got == nil {
			t.Fatalf("GetTask(%s) = %+v, %v", taskID, got, err)
		}
		return got.Status
	}

	bare, bareRun := park("interrupted without evidence", "interrupted")
	if err := store.FinishRun(ctx, identity.TenantID, bareRun.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if got := status(bare.ID); got != "done" {
		t.Fatalf("evidence-free interrupted run projected %q, want settled 'done'", got)
	}

	planned, plannedRun := park("interrupted with plan", "interrupted")
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, plannedRun.ID, "seed", []RunPlanStepInput{{Step: "start", Status: "completed"}, {Step: "finish", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, plannedRun.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if got := status(planned.ID); got != "interrupted" {
		t.Fatalf("interrupted run with evidence projected %q", got)
	}

	dismissed, dismissedRun := park("dismissed park", "waiting_user")
	if err := store.FinishRun(ctx, identity.TenantID, dismissedRun.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if got := status(dismissed.ID); got != "waiting_user" {
		t.Fatalf("parked run projected %q before dismissal", got)
	}
	if ok, err := NewWorkTimeline(store).DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, dismissed.ID, dismissedRun.ID); err != nil || !ok {
		t.Fatalf("dismiss = %v, %v", ok, err)
	}
	if got := status(dismissed.ID); got != "done" {
		t.Fatalf("dismissed run projected %q, want settled 'done'", got)
	}

	asked, askedRun := park("pending approval", "waiting_user")
	if err := store.FinishRun(ctx, identity.TenantID, askedRun.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: asked.ID, RunID: askedRun.ID, ActionType: "tool_call",
	}); err != nil {
		t.Fatal(err)
	}
	if got := status(asked.ID); got != "waiting_user" {
		t.Fatalf("pending approval projected %q", got)
	}
	if _, err := NewWorkTimeline(store).DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, asked.ID, askedRun.ID); !errors.Is(err, ErrAttentionPendingControl) {
		t.Fatalf("dismissal with a pending approval = %v, want ErrAttentionPendingControl", err)
	}
}

// TestPinResumeSelectionIsAtomic covers the explicit /resume write: an archived
// Thread is reopened and both pins land together, and an unknown Thread writes
// nothing at all.
func TestPinResumeSelectionIsAtomic(t *testing.T) {
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
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "shelved work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "shelved work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if err := NewWorkTimeline(store).Archive(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.PinResumeSelection(ctx, identity.TenantID, identity.PersonID, "task_missing", run.ID); err == nil {
		t.Fatal("pinning an unknown thread must fail")
	}
	for _, key := range []string{ResumePinThreadSettingKey, ResumePinRunSettingKey} {
		if value, err := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, key); err != nil || value != "" {
			t.Fatalf("failed pin wrote %s=%q err=%v", key, value, err)
		}
	}

	if err := store.PinResumeSelection(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || reopened == nil || reopened.Visibility != TaskVisibilityListed {
		t.Fatalf("pinned thread = %+v, %v; want reopened", reopened, err)
	}
	if value, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, ResumePinThreadSettingKey); value != task.ID {
		t.Fatalf("thread pin = %q", value)
	}
	if value, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, ResumePinRunSettingKey); value != run.ID {
		t.Fatalf("run pin = %q", value)
	}
	current, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || current == nil || current.ID != task.ID {
		t.Fatalf("CurrentTask after pin = %+v, %v", current, err)
	}
}
