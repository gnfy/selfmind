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

// TestTasksDefaultShowsOpenOnlyAndCollapsesDone: the default view renders open
// labels as multi-line cards and collapses terminal work to a count line.
func TestTasksDefaultShowsOpenOnlyAndCollapsesDone(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	open := seedTask(t, store, identity, "open game build", "in_progress", 3)
	seedTask(t, store, identity, "finished report", "done", 1)
	seedTask(t, store, identity, "old cancelled thing", "cancelled", 0)

	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "[paused] open game build") {
		t.Fatalf("open label card missing:\n%s", out)
	}
	if strings.Contains(out, "finished report") || strings.Contains(out, "old cancelled thing") {
		t.Fatalf("terminal labels must collapse, not list:\n%s", out)
	}
	// The "last:" line carries the latest run's input summary plus the age.
	if !strings.Contains(out, "last: open game build · ") {
		t.Fatalf("last-run summary line missing:\n%s", out)
	}
	if !strings.Contains(out, "runs: 3") {
		t.Fatalf("run count missing:\n%s", out)
	}
	if !strings.Contains(out, "id: "+cardTaskID(open.ID)) {
		t.Fatalf("card id line missing:\n%s", out)
	}
	if !strings.Contains(out, "and 2 done — /tasks done") {
		t.Fatalf("done collapse line missing:\n%s", out)
	}
	// Per-card hints are gone; ONE trailing hint line remains, at the end.
	if got := strings.Count(out, tasksTrailingHint); got != 1 {
		t.Fatalf("want exactly one trailing hint, got %d:\n%s", got, out)
	}
	if !strings.HasSuffix(out, tasksTrailingHint) {
		t.Fatalf("hint must be the last line:\n%s", out)
	}
	// No file line without a handoff; no approvals/questions lines when zero.
	for _, forbidden := range []string{"file:", "approvals:", "questions:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("%q must be omitted when empty:\n%s", forbidden, out)
		}
	}

	// The variants expand what the default collapses, with the same cards.
	done := controlReply(t, daemon, "/tasks done")
	if !strings.Contains(done, "[done] finished report") || !strings.Contains(done, "[cancelled] old cancelled thing") {
		t.Fatalf("/tasks done must list terminal labels with verbatim status:\n%s", done)
	}
	all := controlReply(t, daemon, "/tasks all")
	if !strings.Contains(all, "open game build") || !strings.Contains(all, "finished report") {
		t.Fatalf("/tasks all must list everything:\n%s", all)
	}
}

// TestTasksCardFileApprovalsAndWaiting: the card surfaces the latest handoff's
// primary artifact as a basename, counts pending approvals/questions, and maps
// a label with pending input to [waiting].
func TestTasksCardFileApprovalsAndWaiting(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task := seedTask(t, store, identity, "拳皇97风格对战游戏", "in_progress", 2)

	if _, err := store.SaveHandoff(ctx, control.Handoff{
		TaskID:       task.ID,
		Summary:      "built the arcade page",
		ChangedFiles: []string{"games/arcade-fury-97.html", "games/notes.md"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Question: "which color scheme?",
	}); err != nil {
		t.Fatal(err)
	}

	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "[waiting] 拳皇97风格对战游戏") {
		t.Fatalf("pending approval/question must map to [waiting]:\n%s", out)
	}
	if !strings.Contains(out, "file: arcade-fury-97.html") {
		t.Fatalf("primary artifact basename missing (full path must not leak):\n%s", out)
	}
	if strings.Contains(out, "games/arcade-fury-97.html") {
		t.Fatalf("file line must show the basename only:\n%s", out)
	}
	if !strings.Contains(out, "approvals: 1") || !strings.Contains(out, "questions: 1") {
		t.Fatalf("pending counts missing:\n%s", out)
	}
}

// TestTasksCardInterruptedReplacesAge: an interrupted label's "last:" line ends
// in "· interrupted" instead of the age.
func TestTasksCardInterruptedReplacesAge(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedTask(t, store, identity, "3D 坦克大战重构", "interrupted", 1)

	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "[paused] 3D 坦克大战重构") {
		t.Fatalf("interrupted maps to the paused bracket:\n%s", out)
	}
	if !strings.Contains(out, "last: 3D 坦克大战重构 · interrupted") {
		t.Fatalf("interrupted must replace the age on the last line:\n%s", out)
	}
	if strings.Contains(out, "just now") {
		t.Fatalf("interrupted card must not also render the age:\n%s", out)
	}
}

// TestCardTaskIDRoundTrip: the card's shortened id (`task_` + 8 uuid chars,
// no ellipsis) resolves back through findTaskByRef even when pasted verbatim.
func TestCardTaskIDRoundTrip(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "id roundtrip", "in_progress", 1)

	display := cardTaskID(task.ID)
	// 9 uuid chars minus the group hyphen v4 uuids always have at char 9; no
	// trailing ellipsis (owner preference — it read as noise).
	if strings.HasSuffix(display, ".") || len(display) != len("task_")+8 {
		t.Fatalf("cardTaskID shape wrong: %q", display)
	}
	got, err := daemon.findTaskByRef(context.Background(), identity, display)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != task.ID {
		t.Fatalf("card id %q did not resolve, got %+v", display, got)
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
