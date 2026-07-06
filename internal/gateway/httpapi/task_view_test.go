package httpapi

// /tasks aggregated view + /task subcommands (Work Timeline P3). These go
// through ProcessMessage so every endpoint (CLI, IM) gets identical behavior.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func newTaskViewServer(t *testing.T) (*Server, *control.Store, *control.IdentityContext) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, DefaultTenantID: "default"}, store, identity
}

func controlReply(t *testing.T, daemon *Server, content string) string {
	t.Helper()
	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: content,
	})
	if status != http.StatusOK {
		t.Fatalf("%q: status=%d resp=%+v", content, status, resp)
	}
	if resp.Error != "" {
		t.Fatalf("%q: error=%s", content, resp.Error)
	}
	return resp.Content
}

func seedTask(t *testing.T, store *control.Store, identity *control.IdentityContext, title, status string, runs int) *control.Task {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < runs; i++ {
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
			t.Fatal(err)
		}
	}
	if status != "" {
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	return got
}

// TestTasksDefaultShowsOpenOnlyAndCollapsesDone: the default view lists open
// labels (with run counts and a next-step hint) and collapses terminal work to
// a count line.
func TestTasksDefaultShowsOpenOnlyAndCollapsesDone(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	open := seedTask(t, store, identity, "open game build", "in_progress", 3)
	seedTask(t, store, identity, "finished report", "done", 1)
	seedTask(t, store, identity, "old cancelled thing", "cancelled", 0)

	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "open game build") {
		t.Fatalf("open label missing:\n%s", out)
	}
	if strings.Contains(out, "finished report") || strings.Contains(out, "old cancelled thing") {
		t.Fatalf("terminal labels must collapse, not list:\n%s", out)
	}
	if !strings.Contains(out, "run: 3 次") {
		t.Fatalf("run count missing:\n%s", out)
	}
	if !strings.Contains(out, "and 2 done — /tasks done") {
		t.Fatalf("done collapse line missing:\n%s", out)
	}
	if !strings.Contains(out, "/resume "+shortTaskID(open.ID)) {
		t.Fatalf("paused hint missing:\n%s", out)
	}

	// The variants expand what the default collapses.
	done := controlReply(t, daemon, "/tasks done")
	if !strings.Contains(done, "finished report") || !strings.Contains(done, "old cancelled thing") {
		t.Fatalf("/tasks done must list terminal labels:\n%s", done)
	}
	all := controlReply(t, daemon, "/tasks all")
	if !strings.Contains(all, "open game build") || !strings.Contains(all, "finished report") {
		t.Fatalf("/tasks all must list everything:\n%s", all)
	}
}

// TestTaskDetailRunsRenameArchive drives the /task <id> subcommands end to
// end, including short-prefix id resolution.
func TestTaskDetailRunsRenameArchive(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "detail target", "in_progress", 2)
	short := shortTaskID(task.ID)

	detail := controlReply(t, daemon, "/task "+short)
	for _, want := range []string{"Task: detail target", "ID: " + task.ID, "Runs: 2", "Status:"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q:\n%s", want, detail)
		}
	}

	runsOut := controlReply(t, daemon, "/task "+task.ID+" runs")
	if !strings.Contains(runsOut, "1. [done]") || !strings.Contains(runsOut, "2. [done]") {
		t.Fatalf("runs listing wrong:\n%s", runsOut)
	}

	renamed := controlReply(t, daemon, "/task "+short+" rename KOF fighting game")
	if !strings.Contains(renamed, "Renamed task") {
		t.Fatalf("rename reply: %s", renamed)
	}
	got, _ := store.GetTask(context.Background(), identity.TenantID, task.ID)
	if got.Title != "KOF fighting game" {
		t.Fatalf("title = %q", got.Title)
	}

	archived := controlReply(t, daemon, "/task "+short+" archive")
	if !strings.Contains(archived, "Archived task") {
		t.Fatalf("archive reply: %s", archived)
	}
	got, _ = store.GetTask(context.Background(), identity.TenantID, task.ID)
	if got.Status != "archived" {
		t.Fatalf("status = %q", got.Status)
	}

	// Usage guards.
	if out := controlReply(t, daemon, "/task"); !strings.Contains(out, "Usage: /task") {
		t.Fatalf("bare /task should print usage: %s", out)
	}
	if out := controlReply(t, daemon, "/task task_nope-nope"); !strings.Contains(out, "not found") {
		t.Fatalf("missing task should say not found: %s", out)
	}
}

// TestArchivedExcludedEverywhere: an archived label leaves the /tasks default
// view, recall label cards, and implicit continuation — while /tasks archived
// still shows it.
func TestArchivedExcludedEverywhere(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task := seedTask(t, store, identity, "to be shelved", "in_progress", 1)

	controlReply(t, daemon, "/task "+task.ID+" archive")

	if out := controlReply(t, daemon, "/tasks"); strings.Contains(out, "to be shelved") {
		t.Fatalf("/tasks default must exclude archived:\n%s", out)
	}
	if out := controlReply(t, daemon, "/tasks archived"); !strings.Contains(out, "to be shelved") {
		t.Fatalf("/tasks archived must list it:\n%s", out)
	}
	cards, err := store.ListTaskCards(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range cards {
		if card.TaskID == task.ID {
			t.Fatalf("archived label leaked into recall cards: %+v", card)
		}
	}
	// Implicit continuation never resurrects archived work.
	resumable, err := daemon.resolveContinueTask(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if resumable != nil && resumable.ID == task.ID {
		t.Fatalf("resolveContinueTask offered an archived label: %+v", resumable)
	}
	if !terminalTaskStatus("archived") {
		t.Fatal("archived must be terminal for continuation and pre-label")
	}
}

// TestTaskStatusAliasStillStatus: "/task status" remains the /status alias and
// never falls into the /task <id> handler.
func TestTaskStatusAliasStillStatus(t *testing.T) {
	daemon, _, _ := newTaskViewServer(t)
	out := controlReply(t, daemon, "/task status")
	if strings.Contains(out, "Usage: /task") || strings.Contains(out, "not found") {
		t.Fatalf("/task status should behave like /status, got: %s", out)
	}
}
