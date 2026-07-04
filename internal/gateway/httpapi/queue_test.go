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

	// A continuation cue steers the active task and must never be queued.
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if resp.Turn == nil || resp.Turn.Status != "busy" {
		t.Fatalf("continuation turn = %+v; want status=busy", resp.Turn)
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

	waitUntil(t, 5*time.Second, func() bool {
		n, _ := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		return n == 0
	}, "boot drain did not consume the queued rows")
	waitUntil(t, 5*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		return len(tasks) >= 2
	}, "boot-drained tasks never ran")
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
