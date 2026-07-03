package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// TestAsyncRunMovesCurrentTaskPointer covers the live-use regression where a
// `selfmind send --async` run attached to a channel-matched task but left the
// person's current_task pointer on an unrelated old task, making the user's
// own async run invisible to /status on every endpoint. The pointer must move
// to the task the run actually resolved.
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
	// Task the async send will resolve to (most recent non-terminal task on
	// its channel), created first so the pointer ends up elsewhere.
	asyncTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Async background task",
		Channel:  "send",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Newer task on another channel: CreateTask moves the pointer here, so the
	// async send below resolves a task that differs from current_task.
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

	// The run executes in the background; wait for the pointer to converge.
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && current.ID == asyncTask.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("current task pointer did not move: got %+v, want %s", current, asyncTask.ID)
		}
		time.Sleep(10 * time.Millisecond)
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
	if !strings.Contains(reply, "Running:") || !strings.Contains(reply, run.ID) {
		t.Fatalf("status during active run missing running block: %q", reply)
	}

	daemon.coordinator().endActive(identity.PersonID)
	reply = statusReply()
	if !strings.Contains(reply, "Task: "+pointerTask.Title) {
		t.Fatalf("status after run = %q, want pointer task %q", reply, pointerTask.Title)
	}
}
