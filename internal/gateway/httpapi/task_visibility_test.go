package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// TestAsyncRunMovesCurrentTaskPointer covers the live-use regression where a
// `selfmind send --async` run left the person's current_task pointer on a task
// the run did not resolve, making the async run invisible to /status. Under
// the pre-label semantics (Work Timeline P3) an ordinary async send reuses the
// person's open current label, so the pointer must stay on that label and the
// label must visibly carry the run (last_channel + status move off 'new') —
// the run is never orphaned on an invisible task.
func TestAsyncRunMovesCurrentTaskPointer(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	// Open current label: the async send pre-labels onto it (display guess).
	currentTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Unrelated current task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current == nil || current.ID != currentTask.ID {
		t.Fatalf("test setup: current task = %+v, want %s", current, currentTask.ID)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "send",
		Content:        "keep working on the background refactor",
		Async:          true,
	})
	if status != http.StatusOK || !resp.Accepted {
		t.Fatalf("async accept failed: status=%d resp=%+v", status, resp)
	}

	// The run executes in the background on the pre-labeled current task; wait
	// for the label to visibly carry it (a run stamps last_channel and moves
	// the status off 'new').
	deadline := time.Now().Add(5 * time.Second)
	for {
		after, err := store.GetTask(ctx, identity.TenantID, currentTask.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after != nil && after.LastChannel == "send" && after.Status != "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async run never became visible on the pre-labeled task: got %+v", after)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The pointer stays on the label the run resolved, so /status shows it.
	current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if current == nil || current.ID != currentTask.ID {
		t.Fatalf("pointer should stay on the pre-labeled task; got %+v", current)
	}
}

// TestStatusPrefersActiveRunTask asserts that /status reports the task of the
// person's active run first, falling back to the current_task pointer only
// when nothing is running — so an in-flight async run is always visible.
func TestStatusPrefersActiveRunTask(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	activeTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Async running task",
		Channel:  "send",
	})
	if err != nil {
		t.Fatal(err)
	}
	pointerTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Pointer resting task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, activeTask, "send", "background refactor")
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	if ok := daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		TaskID:    activeTask.ID,
		RunID:     run.ID,
		Channel:   "send",
		StartedAt: time.Now(),
	}); !ok {
		t.Fatal("could not register active run")
	}

	statusReply := func() string {
		resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
			Platform:       "cli",
			PlatformUserID: "local",
			Channel:        "cli",
			Content:        "/status",
		})
		if status != http.StatusOK || resp.Error != "" {
			t.Fatalf("/status failed: status=%d resp=%+v", status, resp)
		}
		return resp.Content
	}

	reply := statusReply()
	// Keep the stable status-card markers (`Task:` / `Status:`) pinned by the
	// continuity eval suite while asserting the active run's task wins.
	if !strings.Contains(reply, "Task: "+activeTask.Title) || !strings.Contains(reply, "Status:") {
		t.Fatalf("status during active run = %q, want task %q", reply, activeTask.Title)
	}
	// The card is conversational: it shows the running state but no run hash
	// (ids live in the control plane; /tasks and the HTTP API expose them).
	if !strings.Contains(reply, "Running:") || strings.Contains(reply, run.ID) {
		t.Fatalf("status during active run missing running block: %q", reply)
	}

	daemon.coordinator().endActive(identity.PersonID)
	reply = statusReply()
	if !strings.Contains(reply, "Task: "+pointerTask.Title) {
		t.Fatalf("status after run = %q, want pointer task %q", reply, pointerTask.Title)
	}
}

// TestStatusSurfacesPendingApproval: a run blocked on an approval must not
// look "stuck" — /status carries the same conversational y/n prompt the push
// uses (observed live: 15 minutes of staring at a silent "running" card).
func TestStatusSurfacesPendingApproval(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
		t.Fatal(err)
	}
	reply, err := daemon.statusReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Waiting for your approval", "reply y or n", "[terminal]", "rm -rf build"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("status card missing %q:\n%s", want, reply)
		}
	}
}
