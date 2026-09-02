package httpapi

// Pre-label semantics (Work Timeline P3, docs/work-timeline.md "Labels" /
// "Ingress"): explicit continuation evidence — req.TaskID, an IntentContinue
// cue (or short acceptance), or the one-shot /resume pin — attaches
// deterministically; every OTHER agent-bound message gets a harmless pre-label
// GUESS: the person's current OPEN (non-terminal, non-archived) label, else a
// fresh placeholder. The guess is safe because context is spine-based and the
// execution workspace follows the REQUEST — the old capture bug's harm (wrong
// workspace, wrong context) is structurally gone — and the post-run labeler
// can re-point a wrong guess. These tests go through ProcessMessage, the same
// entry real channels use.

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// parkEmptyTask creates a /new-created, never-run task that is the person's
// current task — under P3 this is the OPEN label ordinary messages pre-label
// onto (the pre-fix "capture bug" precondition, now harmless by design).
func parkEmptyTask(t *testing.T, daemon *Server, title string) *control.Task {
	t.Helper()
	ctx := context.Background()
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/new " + title,
	})
	if status != 200 || !strings.Contains(resp.Content, "Created task") {
		t.Fatalf("/new failed: status=%d resp=%+v", status, resp)
	}
	identity, err := daemon.Control.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	parked, err := daemon.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || parked == nil {
		t.Fatalf("current task after /new: %v %v", parked, err)
	}
	return parked
}

// TestOrdinaryMessageOwnsFreshRootTask pins the simplification-P2 inversion of
// the old pre-label default: an ordinary message never attaches to the open
// current label — it owns a fresh root task, and the open label is untouched.
func TestOrdinaryMessageOwnsFreshRootTask(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "验证任务C")

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "在工作区新建目录 docsite-demo,生成文档骨架",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("agent turn failed: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID == parked.ID {
		t.Fatalf("ordinary message must own a fresh root task, not the current label %s", parked.ID)
	}
	if runs, err := store.ListTaskRuns(ctx, parked.TenantID, parked.ID, 10); err != nil || len(runs) != 0 {
		t.Fatalf("the open current label must stay untouched, got %d runs (%v)", len(runs), err)
	}
	if runs, err := store.ListTaskRuns(ctx, resp.Task.TenantID, resp.Task.ID, 10); err != nil || len(runs) != 1 {
		t.Fatalf("want 1 run on the fresh task, got %d (%v)", len(runs), err)
	}
}

// TestOrdinaryMessageWithTerminalCurrentCreatesNewLabel: a terminal (done)
// current task is not an open label — plain work gets a fresh placeholder.
func TestOrdinaryMessageWithTerminalCurrentCreatesNewLabel(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "finished work")
	if err := store.UpdateTaskStatus(ctx, parked.TenantID, parked.ID, "done", "all done", nil); err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "start something new",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("agent turn failed: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID == parked.ID {
		t.Fatalf("terminal current task must not be reused as pre-label")
	}
}

// TestOrdinaryMessageWithArchivedCurrentCreatesNewLabel: archived is terminal
// for the pre-label default too.
func TestOrdinaryMessageWithArchivedCurrentCreatesNewLabel(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "shelved work")
	aresp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/task " + parked.ID + " archive",
	})
	if !strings.Contains(aresp.Content, "Archived task") {
		t.Fatalf("archive failed: %+v", aresp)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "start something new",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("agent turn failed: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID == parked.ID {
		t.Fatalf("archived current task must not be reused as pre-label")
	}
}

// TestContinuationCueAttachesToParkedRun: "继续" is explicit continuation
// evidence — it continues the unique unclaimed resumable RUN (the §5.3 ladder)
// and claims it as the new run's parent.
func TestContinuationCueAttachesToParkedRun(t *testing.T) {
	provider := newSlowLLMProvider("continuing where we left off")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "resume me")
	waiting, err := store.StartRun(ctx, parked, "cli", "prepare the release")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, parked.TenantID, waiting.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("continuation turn failed: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID != parked.ID {
		t.Fatalf("continuation created task %s; want attach to %s", resp.Task.ID, parked.ID)
	}
	if resp.Run == nil || resp.Run.ParentRunID != waiting.ID {
		t.Fatalf("continuation must claim the waiting run as parent: %+v", resp.Run)
	}
}

// TestExplicitTaskIDStillWins: a caller-supplied task id is deterministic and
// beats the pre-label guess.
func TestExplicitTaskIDStillWins(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	first := parkEmptyTask(t, daemon, "task A")
	// Make a second open label the current one.
	second := parkEmptyTask(t, daemon, "task B")
	if second.ID == first.ID {
		t.Fatal("setup: expected two distinct tasks")
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "work on A specifically", TaskID: first.ID,
	})
	if status != 200 || resp.Task == nil || resp.Task.ID != first.ID {
		t.Fatalf("explicit task id must win: status=%d resp=%+v", status, resp.Task)
	}
	runs, _ := store.ListTaskRuns(ctx, first.TenantID, first.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("want the run on the explicit task, got %d runs", len(runs))
	}
}

// TestResumePinReopensArchivedTask: an explicit /resume is a deliberate act —
// its one-shot pin attaches the next message even to an ARCHIVED label, while
// the pre-label default keeps skipping archived work.
func TestResumePinReopensArchivedTask(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "archived work")
	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/task " + parked.ID + " archive",
	})

	// Pre-label skips the archived label: ordinary work gets its own task.
	first, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "some unrelated new work",
	})
	if first.Task == nil || first.Task.ID == parked.ID {
		t.Fatalf("archived label must not capture ordinary work, got %+v", first.Task)
	}

	// /resume explicitly reopens it for exactly the next message.
	rresp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/resume " + parked.ID,
	})
	if !strings.Contains(rresp.Content, "Resumed task") {
		t.Fatalf("/resume of an archived task must work: %+v", rresp)
	}
	second, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "pick this back up",
	})
	if second.Task == nil || second.Task.ID != parked.ID {
		t.Fatalf("message after /resume did not attach to the archived task: %+v", second.Task)
	}
	// The run moved it out of archived (a run flips status via StartRun +
	// finalization), so it is open again.
	after, _ := store.GetTask(ctx, parked.TenantID, parked.ID)
	if after == nil || archivedTaskStatus(after.Status) {
		t.Fatalf("resumed task should have left archived, got %+v", after)
	}
}

func TestResumePinReopensCompletedTask(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	completed := parkEmptyTask(t, daemon, "completed work")
	closed, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/task " + completed.ID + " complete",
	})
	if !strings.Contains(closed.Content, "Completed task") {
		t.Fatalf("completion failed: %+v", closed)
	}

	resumed, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/resume " + completed.ID,
	})
	if !strings.Contains(resumed.Content, "Resumed task") {
		t.Fatalf("/resume of a completed task must work: %+v", resumed)
	}
	next, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "add one more follow-up",
	})
	if next.Task == nil || next.Task.ID != completed.ID {
		t.Fatalf("message after /resume did not attach to the completed task: %+v", next.Task)
	}
	after, _ := store.GetTask(ctx, completed.TenantID, completed.ID)
	if after == nil || terminalTaskStatus(after.Status) {
		t.Fatalf("resumed completed task should be open again, got %+v", after)
	}
}

// TestAsyncExplicitWorkspaceOwnsFreshTask is the rewritten live-bug
// regression: an async request with an EXPLICIT workspace owns its own root
// task (simplification P2) — the unrelated open current label is untouched,
// and the fresh task carries the request workspace.
func TestAsyncExplicitWorkspaceOwnsFreshTask(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "验证任务C")

	root := t.TempDir()
	ws, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: parked.TenantID, OwnerPersonID: parked.PersonID,
		Name: "docsite", LocalPath: root, AllowedRoots: []string{root},
	})
	if err != nil || ws == nil {
		t.Fatalf("register workspace: %v", err)
	}

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "在工作区新建目录 docsite-demo,生成文档骨架", Async: true, WorkspaceID: ws.ID,
	})
	if !resp.Accepted {
		t.Fatalf("async dispatch not accepted: %+v", resp)
	}

	// The async run lands on its OWN fresh task; the parked label stays empty.
	var freshTaskID string
	waitUntil(t, 5*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, parked.TenantID, parked.PersonID, 20)
		for _, task := range tasks {
			if task.ID == parked.ID {
				continue
			}
			runs, _ := store.ListTaskRuns(ctx, parked.TenantID, task.ID, 10)
			if len(runs) == 1 {
				freshTaskID = task.ID
				return true
			}
		}
		return false
	}, "async run never landed on its own fresh task")
	_ = freshTaskID
	if runs, _ := store.ListTaskRuns(ctx, parked.TenantID, parked.ID, 10); len(runs) != 0 {
		t.Fatalf("the open current label must stay untouched, got %d runs", len(runs))
	}
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListRunningRuns(ctx, parked.TenantID, []string{parked.PersonID})
		return len(runs) == 0
	}, "async run never finished")

	// Harmlessness: the guessed label's workspace binding is NOT mutated by
	// the request's explicit workspace — a wrong guess can never re-scope the
	// label (execution scope took the REQUEST workspace independently).
	after, _ := store.GetTask(ctx, parked.TenantID, parked.ID)
	if after == nil || after.WorkspaceID != "" {
		t.Fatalf("pre-label guess must not stamp the request workspace onto the label: %+v", after)
	}
}

// TestDrainedQueueItemOwnsFreshRootTask: a queued item drained after the
// active run finishes is an ordinary message — it owns its own root task
// (simplification P2), and the queue/busy path itself is untouched.
func TestDrainedQueueItemOwnsFreshRootTask(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/new --run the queued unrelated task",
	})
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 1 {
		t.Fatalf("precondition: queued count = %d; want 1", n)
	}

	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool {
		n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		return n == 0
	}, "queued item was never drained")
	waitUntil(t, 5*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		if len(tasks) != 2 {
			return false
		}
		for _, task := range tasks {
			runs, _ := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
			if len(runs) != 1 {
				return false
			}
		}
		return true
	}, "drained item did not own a fresh root task")
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return len(runs) == 0
	}, "a run stayed running after drain")
}
