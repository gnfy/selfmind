package httpapi

// G1+G2: queue new work instead of rejecting it as "busy", and auto-start the
// next queued item when a run finishes. These tests reuse the detached-run
// harness (newDetachedRunServer / slowLLMProvider / waitUntil).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// startBlockedRun begins a slow async run for cli/local and waits until the
// agent turn is actually executing, so the person has a genuine active run.
func startBlockedRun(t *testing.T, daemon *Server, provider *slowLLMProvider) *control.IdentityContext {
	t.Helper()
	ctx := context.Background()
	identity, err := daemon.Control.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "first slow task", Async: true,
	})
	if !resp.Accepted {
		t.Fatalf("first async run not accepted: %+v", resp)
	}
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first run never reached the provider")
	}
	return identity
}

func TestNewWorkQueuesWhenBusy(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "a second unrelated task",
	})
	if resp.Turn == nil || resp.Turn.Status != "queued" {
		t.Fatalf("second message turn = %+v; want status=queued", resp.Turn)
	}
	if !resp.Accepted {
		t.Fatalf("queued message should be accepted")
	}
	if !strings.Contains(resp.Content, "Queued behind the running task") {
		t.Fatalf("queued reply = %q; want the honest acceptance line", resp.Content)
	}
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 1 {
		t.Fatalf("queued count = %d; want 1", n)
	}
}

func TestContinuationDoesNotQueue(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	// A continuation cue steers the ACTIVE run (injected into its steering
	// channel, the same one /v1/runs/steer uses) and must never be queued. The
	// run has a live steering channel with buffer space, so the continuation is
	// accepted, not bounced as "busy" — this is the cross-endpoint takeover the
	// north star requires (a continuation from any surface reaches the run).
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if resp.Turn == nil || resp.Turn.Status != "accepted" {
		t.Fatalf("continuation turn = %+v; want status=accepted (steered)", resp.Turn)
	}
	if !resp.Accepted {
		t.Fatalf("continuation should be accepted (steered into the run); got %+v", resp)
	}
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 0 {
		t.Fatalf("continuation must not enqueue; queued count = %d", n)
	}
}

func TestDrainAutoStartsNextQueued(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	// Queue a second task behind the running one.
	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "the queued task",
	})
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 1 {
		t.Fatalf("precondition: queued count = %d; want 1", n)
	}

	// Release the provider so the first run finishes, which drains the queue.
	provider.releaseNow()

	// The queued item is drained (no longer 'queued') and eventually two tasks exist.
	waitUntil(t, 5*time.Second, func() bool {
		n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		return n == 0
	}, "queued item was never drained")
	waitUntil(t, 5*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		return len(tasks) >= 2
	}, "drained task never ran")
	// No run should be left executing.
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return len(runs) == 0
	}, "a run stayed running after drain")
}

func TestQueueSurvivesRestartBootDrain(t *testing.T) {
	provider := newSlowLLMProvider("done")
	provider.releaseNow() // fast turns for the boot-drained items
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate rows that were pending when a previous daemon died: one 'queued'
	// and one 'started' (mid-launch at crash — must be requeued and run).
	q1, _ := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "survivor one"})
	q2, _ := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "survivor two"})
	_ = q1
	_ = store.MarkQueued(ctx, identity.TenantID, q2.ID, control.QueueStatusStarted)

	// A fresh Server (new coordinator) with the same store == a restart.
	daemon.DrainQueuedAtBoot(ctx)

	// Generous deadlines: the drain chains two full async runs (drain -> run ->
	// finalize -> drain next), and under full-suite CPU contention 5s flaked
	// (passes in isolation). waitUntil polls, so green runs stay fast.
	waitUntil(t, 30*time.Second, func() bool {
		n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		return n == 0
	}, "boot drain did not consume the queued rows")
	waitUntil(t, 30*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		return len(tasks) >= 2
	}, "boot-drained tasks never ran")
}

// TestDrainedItemMarkedDoneNotRequeued pins the duplicate-execution fix: a
// queued item that is drained AND COMPLETES must end 'done' (not left 'started')
// so a later boot drain never re-runs the finished work.
func TestDrainedItemMarkedDoneNotRequeued(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	// Queue a second task behind the running one, capture its row id.
	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "the queued task",
	})
	queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if len(queued) != 1 {
		t.Fatalf("precondition: queued rows = %d; want 1", len(queued))
	}
	rowID := queued[0].ID

	// Release the provider: the running run finishes, drains the queue, and the
	// drained run then completes too.
	provider.releaseNow()

	// The drained row transitions to 'done' (not stuck on 'started').
	waitUntil(t, 10*time.Second, func() bool {
		r, _ := store.GetQueued(ctx, identity.TenantID, rowID)
		return r != nil && r.Status == control.QueueStatusDone
	}, "drained-and-completed row never reached 'done'")

	// No run left executing.
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return len(runs) == 0
	}, "a run stayed running after drain")

	tasksBefore, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 50)

	// A boot drain must NOT resurrect the completed work: the done row is neither
	// 'started' (requeued) nor 'queued' (drained), so nothing re-runs.
	daemon.DrainQueuedAtBoot(ctx)

	if r, _ := store.GetQueued(ctx, identity.TenantID, rowID); r == nil || r.Status != control.QueueStatusDone {
		t.Fatalf("done row must survive boot drain unchanged, got %+v", r)
	}
	// Give any erroneous drain a beat, then assert the task count did not grow.
	time.Sleep(200 * time.Millisecond)
	tasksAfter, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 50)
	if len(tasksAfter) != len(tasksBefore) {
		t.Fatalf("boot drain re-ran completed work: tasks %d -> %d", len(tasksBefore), len(tasksAfter))
	}
}

func TestQueueControlCommandsListAndClear(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, _ := daemon.Control.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "queued alpha"})

	handled, reply, err := daemon.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: "cli", Content: "/queue"})
	if !handled || err != nil {
		t.Fatalf("/queue handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "queued alpha") {
		t.Fatalf("/queue reply = %q; want the queued content", reply)
	}

	handled, reply, err = daemon.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: "cli", Content: "/queue clear"})
	if !handled || err != nil {
		t.Fatalf("/queue clear handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "Cleared 1") {
		t.Fatalf("/queue clear reply = %q; want Cleared 1", reply)
	}
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 0 {
		t.Fatalf("queue not cleared; count = %d", n)
	}
}

func TestStopThenDrain(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity := startBlockedRun(t, daemon, provider)

	daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "task behind the stopped one",
	})
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 1 {
		t.Fatalf("precondition queued count = %d; want 1", n)
	}

	// /stop cancels the active run; its finalization drains the queue.
	handled, _, err := daemon.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: "cli", Content: "/stop"})
	if !handled || err != nil {
		t.Fatalf("/stop handled=%v err=%v", handled, err)
	}
	// Let the drained item complete too.
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool {
		n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		return n == 0
	}, "queue not drained after /stop")
	// Let the drained run finish before the test's temp store is torn down, so
	// no background goroutine writes to a closed DB.
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return len(runs) == 0
	}, "a run stayed running after /stop drain")
}

func TestDiagControlCommand(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, _ := daemon.Control.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "queued thing"})

	handled, reply, err := daemon.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: "cli", Content: "/diag"})
	if !handled || err != nil {
		t.Fatalf("/diag handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "SelfMind diagnostics") {
		t.Fatalf("/diag reply missing header: %q", reply)
	}
	if !strings.Contains(reply, "Queued: 1") {
		t.Fatalf("/diag reply = %q; want Queued: 1", reply)
	}
	if !strings.Contains(reply, "Tasks: open 0, terminal 0, archived 0, pinned 0, inbox runs 0") {
		t.Fatalf("/diag reply = %q; want task governance stats", reply)
	}
	if !strings.Contains(reply, "Active run: none") {
		t.Fatalf("/diag reply = %q; want Active run: none", reply)
	}
}

// TestUnknownSlashCommandIsRejectedNotQueued reproduces the live bug: a
// slash-shaped message that no control command claims ("/qwer") must be
// rejected as a mistyped command — never dispatched to the agent or queued
// behind a running task.
func TestUnknownSlashCommandIsRejectedNotQueued(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: identity.Platform, PlatformUserID: identity.PlatformUserID,
		Content: "/qwer",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(resp.Content, "Unknown command /qwer") {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Accepted {
		t.Fatal("unknown command must not be accepted as work")
	}
	// Nothing was queued.
	if n, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || n != 0 {
		t.Fatalf("queued = %d err = %v; unknown slash must not enqueue", n, err)
	}
}

// TestStopCancelsStuckTaskWhenNoRun covers the live gap: a task stuck
// non-terminal with no active run (e.g. created but never executed) must be
// cancellable via /stop's no-run fallback (and /cancel), instead of sitting
// in /tasks forever ("No active run to stop" with no way to clear it).
func TestStopCancelsStuckTaskWhenNoRun(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "stuck /qwer task", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "in_progress", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: identity.Platform, PlatformUserID: identity.PlatformUserID, Content: "/stop",
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "cancelled the current task") {
		t.Fatalf("stop no-run fallback: status=%d content=%q", status, resp.Content)
	}
	got, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	if got == nil || got.Status != "cancelled" {
		t.Fatalf("task should be cancelled, got %+v", got)
	}

	// A second /stop with the task already terminal is a clean no-op message.
	resp, _ = daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: identity.Platform, PlatformUserID: identity.PlatformUserID, Content: "/stop",
	})
	if !strings.Contains(resp.Content, "already cancelled") && !strings.Contains(resp.Content, "no current task") {
		t.Fatalf("second stop should report terminal/none, got %q", resp.Content)
	}
}

// TestBootRequeueIsCapped reproduces the live resurrection loop: a 'started'
// row whose run never finalizes before the next daemon restart must be
// requeued at most maxQueueRestarts times, then dropped as failed — not
// resurrected at every boot forever (observed live: five duplicate task
// corpses after a day of deploy restarts).
func TestBootRequeueIsCapped(t *testing.T) {
	provider := newSlowLLMProvider("done")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "long survivor"})
	_ = store.MarkQueued(ctx, identity.TenantID, q.ID, control.QueueStatusStarted)

	// Boot 1: within budget → requeued.
	requeued, dropped, err := store.RequeueStartedQueued(ctx)
	if err != nil || requeued != 1 || dropped != 0 {
		t.Fatalf("boot1 requeue = %d/%d, %v; want 1/0", requeued, dropped, err)
	}
	// Simulate it launching again and the daemon dying again.
	_ = store.MarkQueued(ctx, identity.TenantID, q.ID, control.QueueStatusStarted)

	// Boot 2: budget exhausted → dropped as failed, never requeued.
	requeued, dropped, err = store.RequeueStartedQueued(ctx)
	if err != nil || requeued != 0 || dropped != 1 {
		t.Fatalf("boot2 requeue = %d/%d, %v; want 0/1", requeued, dropped, err)
	}
	if n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); n != 0 {
		t.Fatalf("row must not be queued after budget exhaustion, queued=%d", n)
	}
	_ = daemon
}

// TestDrainCancelsQueuedSlashCommands: poison rows (slash content enqueued
// before the unknown-slash gate existed) are cancelled at drain time, never
// launched as agent runs; the next real item still drains.
func TestDrainCancelsQueuedSlashCommands(t *testing.T) {
	provider := newSlowLLMProvider("done")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	junk, _ := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/qwer"})
	real, _ := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "real queued work"})

	daemon.DrainQueuedAtBoot(ctx)

	waitUntil(t, 30*time.Second, func() bool {
		j, _ := store.GetQueued(ctx, identity.TenantID, junk.ID)
		return j != nil && j.Status == control.QueueStatusCancelled
	}, "slash row was not cancelled at drain")
	waitUntil(t, 30*time.Second, func() bool {
		r, _ := store.GetQueued(ctx, identity.TenantID, real.ID)
		// Drained means it left 'queued': it is 'started' while its run executes
		// and 'done' once the run finalizes (fast here, so either is acceptable).
		return r != nil && (r.Status == control.QueueStatusStarted || r.Status == control.QueueStatusDone)
	}, "real row after the slash row never drained")
	// The junk must never have become a task.
	tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
	for _, task := range tasks {
		if strings.Contains(task.Title, "/qwer") {
			t.Fatalf("queued slash command became a task: %+v", task)
		}
	}
}
