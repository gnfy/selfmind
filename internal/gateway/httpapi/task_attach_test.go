package httpapi

// Task-attach semantics for parked tasks (docs/STATUS.md P0, 2026-07-05):
// a message attaches to an existing task ONLY on explicit continuation
// evidence — req.TaskID, an IntentContinue cue (or short-acceptance upgrade),
// or the one-shot /resume pin. Any other message that reaches the agent —
// sync, async, and queued-drained alike — creates its OWN task even while a
// parked non-terminal task exists, and the parked task stays resumable.
// These tests reuse the detached-run harness and go through ProcessMessage,
// the same entry real channels use.

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// parkEmptyTask reproduces the live defect precondition: a /new-created,
// never-run task that is the person's current task ("验证任务C").
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

func TestNewImperativeMessageCreatesNewTaskNotAttach(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "验证任务C")

	// A brand-new imperative request must NOT land on the parked current task.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "在工作区新建目录 docsite-demo,生成文档骨架",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("agent turn failed: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID == parked.ID {
		t.Fatalf("new imperative message attached to the parked task %s", parked.ID)
	}

	// The parked task is untouched (still 'new', never run)…
	after, err := store.GetTask(ctx, parked.TenantID, parked.ID)
	if err != nil || after == nil {
		t.Fatalf("parked task lookup: %v %v", after, err)
	}
	if after.Status != parked.Status || after.Title != parked.Title {
		t.Fatalf("parked task mutated: before=%+v after=%+v", parked, after)
	}
	// …and still resumable via /resume.
	rresp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/resume " + parked.ID,
	})
	if !strings.Contains(rresp.Content, "Resumed task") {
		t.Fatalf("/resume of the parked task failed: %+v", rresp)
	}
}

func TestContinuationCueAttachesToParkedTask(t *testing.T) {
	provider := newSlowLLMProvider("continuing where we left off")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "resume me")

	// "继续" is continuation evidence: it must attach, never create a task.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("continuation turn failed: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID != parked.ID {
		t.Fatalf("continuation created task %s; want attach to %s", resp.Task.ID, parked.ID)
	}
}

func TestResumePinAttachesExactlyOneFollowingMessage(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "pinned work")

	// Move the current pointer off the parked task with an ordinary turn.
	first, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "some unrelated new work",
	})
	if first.Task == nil || first.Task.ID == parked.ID {
		t.Fatalf("precondition: ordinary turn should have its own task, got %+v", first.Task)
	}

	// /resume pins the parked task for exactly the next message.
	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/resume " + parked.ID,
	})
	second, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "add the tests we discussed",
	})
	if second.Task == nil || second.Task.ID != parked.ID {
		t.Fatalf("message after /resume did not attach to the resumed task: %+v", second.Task)
	}

	// The pin is one-shot: the following plain message is new work again.
	third, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "now a different request entirely",
	})
	if third.Task == nil || third.Task.ID == parked.ID {
		t.Fatalf("stale /resume pin captured a later message: %+v", third.Task)
	}
}

func TestAsyncExplicitWorkspaceLandsOnNewTask(t *testing.T) {
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

	// The async run must create its own task carrying the explicit workspace.
	var created *control.Task
	waitUntil(t, 5*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, parked.TenantID, parked.PersonID, 20)
		for i := range tasks {
			if tasks[i].ID != parked.ID {
				created = &tasks[i]
				return true
			}
		}
		return false
	}, "async run never created its own task")
	if created.WorkspaceID != ws.ID {
		t.Fatalf("async task workspace = %q; want explicit %q", created.WorkspaceID, ws.ID)
	}
	if after, _ := store.GetTask(ctx, parked.TenantID, parked.ID); after == nil || after.Status != "new" {
		t.Fatalf("parked task mutated by the async dispatch: %+v", after)
	}
}

func TestDrainedQueueItemCreatesOwnTask(t *testing.T) {
	// A non-terminal answer parks the first task as in_progress, so this also
	// proves the drained item does not attach to a live parked task.
	provider := newSlowLLMProvider("making progress on the requested work")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "the queued unrelated task",
	})
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 1 {
		t.Fatalf("precondition: queued count = %d; want 1", n)
	}

	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool {
		n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		return n == 0
	}, "queued item was never drained")
	// The drained item becomes its own async run with its own task; wait for
	// that task to exist before asserting (the 'started' queue row precedes
	// the run row).
	waitUntil(t, 5*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		return len(tasks) >= 2
	}, "drained item never created its own task")
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return len(runs) == 0
	}, "a run stayed running after drain")

	tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
	if len(tasks) != 2 {
		t.Fatalf("want 2 distinct tasks (finisher + drained), got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].ID == tasks[1].ID {
		t.Fatalf("drained item reused the finisher's task %s", tasks[0].ID)
	}
	// The first task parked non-terminal, so the drained item had a live
	// parked current task to wrongly attach to — and did not.
	sawParked := false
	for _, task := range tasks {
		if !terminalTaskStatus(task.Status) {
			sawParked = true
		}
	}
	if !sawParked {
		t.Fatalf("test lost its premise: no non-terminal parked task among %+v", tasks)
	}
}
