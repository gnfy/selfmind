package httpapi

// /tasks aggregated view + /task subcommands (Work Timeline P3). These go
// through ProcessMessage so every endpoint (CLI, IM) gets identical behavior.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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
	effectiveRuns := runs
	switch status {
	case "in_progress", "interrupted", "waiting_user", "verification_partial", "blocked":
		if effectiveRuns == 0 {
			effectiveRuns = 1
		}
	}
	var lastStarted time.Time
	for i := 0; i < effectiveRuns; i++ {
		if i == effectiveRuns-1 && i > 0 {
			// Run clocks have second precision and same-second runs order by
			// random id; the parked run must be unambiguously the latest.
			advanceRunClock(lastStarted)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		lastStarted = run.StartedAt
		runStatus := "done"
		if i == effectiveRuns-1 {
			switch status {
			case "in_progress":
				// A parked turn waits for the person: Attention without any
				// further evidence.
				runStatus = "waiting_user"
			case "interrupted":
				// An interrupted Run is Attention only with work evidence; a
				// persisted MULTI-STEP plan is the smallest durable proof that
				// this Run was doing ongoing work when it was cut off.
				if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "seed", []control.RunPlanStepInput{
					{Step: "start " + title, Status: "completed"},
					{Step: "continue " + title, Status: "in_progress"},
				}); err != nil {
					t.Fatal(err)
				}
				runStatus = status
			case "waiting_user", "verification_partial", "blocked", "completed", "cancelled", "failed":
				runStatus = status
			}
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, runStatus); err != nil {
			t.Fatal(err)
		}
	}
	if status == "archived" {
		if err := control.NewWorkTimeline(store).Archive(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	return got
}

// advanceRunClock blocks until the wall clock has left the second in which
// `since` was recorded, so the next StartRun sorts strictly after it. Run
// timestamps have second precision, and the Attention "latest Run" rule breaks
// same-second ties by id, which is random.
func advanceRunClock(since time.Time) {
	for time.Now().Unix() <= since.Unix() {
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTasksDefaultShowsOpenOnlyAndCollapsesDone: the default view renders open
// labels as multi-line cards and collapses terminal work to a count line.
func TestTasksDefaultShowsOpenOnlyAndCollapsesDone(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	open := seedTask(t, store, identity, "open game build", "in_progress", 3)
	seedTask(t, store, identity, "finished report", "done", 1)
	seedTask(t, store, identity, "old cancelled thing", "cancelled", 0)

	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "[resumable] open game build") {
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
	if !strings.Contains(done, "[done] finished report") || !strings.Contains(done, "[done] old cancelled thing") {
		t.Fatalf("/tasks done must list settled Threads without inventing an aggregate cancellation status:\n%s", done)
	}
	all := controlReply(t, daemon, "/tasks all")
	if !strings.Contains(all, "open game build") || !strings.Contains(all, "finished report") {
		t.Fatalf("/tasks all must list everything:\n%s", all)
	}
}

func TestTasksAttentionOrdinalPinsTheExactResumableRun(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "release waiting for confirmation", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "release waiting for confirmation")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}

	listed := controlReply(t, daemon, "/tasks")
	if !strings.Contains(listed, "resume: "+shortRunID(run.ID)) {
		t.Fatalf("attention card did not expose its exact run:\n%s", listed)
	}
	selected := controlReply(t, daemon, "/resume 1")
	if !strings.Contains(selected, "Selected run "+shortRunID(run.ID)) {
		t.Fatalf("ordinal did not select exact run: %s", selected)
	}
	pinned, err := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumeRunPinKey)
	if err != nil || pinned != run.ID {
		t.Fatalf("resume run pin=%q err=%v, want %s", pinned, err, run.ID)
	}
}

// seedTwoAttentionRunsInOneThread parks two Runs in one Thread that are both
// Attention: the older one keeps a pending approval (a newer Run supersedes
// its resumable state, but not its open decision), the newer one is the
// Thread's resumable Run. Run clocks have second precision, so the newer Run
// is started in the next second to make "latest" deterministic.
func seedTwoAttentionRunsInOneThread(t *testing.T, store *control.Store, identity *control.IdentityContext, title string) (*control.Task, *control.Run, *control.Run) {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.StartRun(ctx, task, "cli", "confirm api release")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, older.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: older.ID, ActionType: "tool_call",
	}); err != nil {
		t.Fatal(err)
	}
	advanceRunClock(older.StartedAt)
	newer, err := store.StartRun(ctx, task, "cli", "confirm worker release")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, newer.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	return task, older, newer
}

func TestTasksKeepsTwoAttentionRunsInOneThreadDistinct(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	_, older, newer := seedTwoAttentionRunsInOneThread(t, store, identity, "two release confirmations")
	attention, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 2 {
		t.Fatalf("attention=%+v err=%v", attention, err)
	}
	// The open decision ranks before the resumable Run; the superseded older
	// Run is Attention only through its approval.
	if attention[0].RunID != older.ID || attention[0].Activity != control.ThreadActivityNeedsAttention ||
		attention[1].RunID != newer.ID || attention[1].Activity != control.ThreadActivityResumable {
		t.Fatalf("attention order/activity = %+v", attention)
	}
	listed := controlReply(t, daemon, "/tasks")
	for _, item := range attention {
		if !strings.Contains(listed, "resume: "+shortRunID(item.RunID)) || !strings.Contains(listed, item.RunSummary) {
			t.Fatalf("exact attention run missing from cards: item=%+v\n%s", item, listed)
		}
	}
	selected := controlReply(t, daemon, "/resume 1")
	if !strings.Contains(selected, "Selected run "+shortRunID(attention[0].RunID)) {
		t.Fatalf("ordinal drifted from rendered attention order: %s", selected)
	}
}

func TestTaskAttentionOrdinalDismissesOnlyItsExactRun(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	_, older, newer := seedTwoAttentionRunsInOneThread(t, store, identity, "two independently actionable runs")
	before, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(before) != 2 || before[0].RunID != older.ID || before[1].RunID != newer.ID {
		t.Fatalf("attention before=%+v err=%v", before, err)
	}
	_ = controlReply(t, daemon, "/tasks")
	// Card 1 still owns a pending approval: it refuses, and nothing changes.
	if refused := controlReply(t, daemon, "/task 1 complete"); !strings.Contains(refused, "pending approval") {
		t.Fatalf("card with a pending approval was not refused: %s", refused)
	}
	got := controlReply(t, daemon, "/task 2 complete")
	if !strings.Contains(got, shortRunID(newer.ID)) {
		t.Fatalf("dismiss reply did not identify exact run %s: %s", newer.ID, got)
	}
	after, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(after) != 1 || after[0].RunID != older.ID {
		t.Fatalf("ordinal dismissal crossed run boundary: before=%+v after=%+v err=%v", before, after, err)
	}
}

// TestTaskCompleteDismissesAttentionWithoutClosingRunOrApproval: dismissal is
// refused while the exact Run still owns a pending approval. The Run, the
// approval, and the Attention row all stay exactly as they were.
func TestTaskCompleteDismissesAttentionWithoutClosingRunOrApproval(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "work finished elsewhere", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", task.Title)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = controlReply(t, daemon, "/tasks")
	reply := controlReply(t, daemon, "/task 1 complete")
	if !strings.Contains(reply, "pending approval, clarification, or watcher") || !strings.Contains(reply, "attention is not dismissed") {
		t.Fatalf("completion did not refuse a run with a pending approval: %s", reply)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "waiting_user" {
		t.Fatalf("refused completion rewrote run: %+v err=%v", storedRun, err)
	}
	storedApproval, err := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if err != nil || storedApproval == nil || storedApproval.PersonID != identity.PersonID || storedApproval.Status != "pending" {
		t.Fatalf("refused completion resolved approval: %+v err=%v", storedApproval, err)
	}
	attention, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 1 || attention[0].RunID != run.ID {
		t.Fatalf("attention changed despite refusal: %+v err=%v", attention, err)
	}
	// Answering the decision makes the same command succeed.
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "rejected", "cli", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if reply := controlReply(t, daemon, "/task 1 complete"); !strings.Contains(reply, "Dismissed current attention") {
		t.Fatalf("completion after the decision was answered: %s", reply)
	}
	if attention, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10); err != nil || len(attention) != 0 {
		t.Fatalf("attention remained after dismissal: %+v err=%v", attention, err)
	}
}

func TestTaskArchiveHidesThreadWithoutCancellingControlState(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "archive presentation only", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", task.Title)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := controlReply(t, daemon, "/task "+task.ID+" archive")
	if !strings.Contains(reply, "Archived thread") {
		t.Fatalf("archive reply=%s", reply)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || stored == nil || stored.Visibility != control.TaskVisibilityArchived {
		t.Fatalf("archived thread=%+v err=%v", stored, err)
	}
	storedRun, _ := store.GetRun(ctx, identity.TenantID, run.ID)
	storedApproval, _ := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if storedRun == nil || storedRun.Status != "waiting_user" || storedApproval == nil || storedApproval.Status != "pending" {
		t.Fatalf("archive changed control state: run=%+v approval=%+v", storedRun, storedApproval)
	}
}

// TestTasksCardFileApprovalsAndWaiting: the card surfaces the latest handoff's
// primary artifact as a basename, counts pending approvals/questions, and maps
// a label with pending input to [waiting].
func TestTasksCardFileApprovalsAndWaiting(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task := seedTask(t, store, identity, "拳皇97风格对战游戏", "in_progress", 2)
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("seed run: %+v err=%v", runs, err)
	}

	if _, err := store.SaveHandoff(ctx, control.Handoff{
		TaskID:       task.ID,
		Summary:      "built the arcade page",
		ChangedFiles: []string{"games/arcade-fury-97.html", "games/notes.md"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: runs[0].ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: runs[0].ID,
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

// TestTasksCardShowsInterruptedRunAsResumable presents actionable state rather
// than turning a historical Run status into a Thread status.
func TestTasksCardShowsInterruptedRunAsResumable(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedTask(t, store, identity, "3D 坦克大战重构", "interrupted", 1)

	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "[resumable] 3D 坦克大战重构") {
		t.Fatalf("interrupted run must be presented as resumable:\n%s", out)
	}
	if !strings.Contains(out, "resume: run_") {
		t.Fatalf("resumable card must name its exact Run:\n%s", out)
	}
}

func TestTasksSearchFiltersAcrossTaskCards(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	open := seedTask(t, store, identity, "KOF 97 battle game", "in_progress", 1)
	seedTask(t, store, identity, "pgsql example", "done", 1)
	seedTask(t, store, identity, "unrelated notes", "done", 1)
	archived := seedTask(t, store, identity, "old tank battle", "archived", 1)

	if _, err := store.SaveHandoff(ctx, control.Handoff{
		TaskID:       open.ID,
		Summary:      "arcade work",
		ChangedFiles: []string{"games/arcade-fury-97.html"},
	}); err != nil {
		t.Fatal(err)
	}

	out := controlReply(t, daemon, "/tasks arcade-fury")
	if !strings.Contains(out, "KOF 97 battle game") || !strings.Contains(out, "Use /resume <id>") {
		t.Fatalf("search should match handoff file and show id hint:\n%s", out)
	}
	if strings.Contains(out, "pgsql example") || strings.Contains(out, "unrelated notes") {
		t.Fatalf("search leaked unrelated tasks:\n%s", out)
	}

	doneOut := controlReply(t, daemon, "/tasks done pgsql")
	if !strings.Contains(doneOut, "pgsql example") || strings.Contains(doneOut, "KOF 97 battle game") {
		t.Fatalf("done-scoped search wrong:\n%s", doneOut)
	}

	archivedOut := controlReply(t, daemon, "/tasks archived tank")
	if !strings.Contains(archivedOut, archived.Title) {
		t.Fatalf("archived-scoped search missing task:\n%s", archivedOut)
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

func TestTaskOrdinalResolvesCardOrder(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedTask(t, store, identity, "first open task", "in_progress", 1)
	seedTask(t, store, identity, "second open task", "in_progress", 1)
	seedTask(t, store, identity, "finished work", "done", 1)

	tasks, err := daemon.listTasksForDisplay(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	open := tasks
	if len(open) != 2 {
		t.Fatalf("want 2 open tasks, got %d", len(open))
	}

	// The card numbered 1 by /tasks is the task /task 1 and /resume 1 resolve.
	out := controlReply(t, daemon, "/tasks")
	if !strings.Contains(out, "1. [resumable] "+open[0].Title) {
		t.Fatalf("/tasks card 1 disagrees with display order:\n%s", out)
	}
	detail := controlReply(t, daemon, "/task 1")
	if !strings.Contains(detail, "ID: "+open[0].ID) {
		t.Fatalf("/task 1 resolved the wrong task:\n%s", detail)
	}
	detail2 := controlReply(t, daemon, "/task 2 runs")
	if !strings.Contains(detail2, "1. [waiting_user]") {
		t.Fatalf("/task 2 runs should list the second card's runs:\n%s", detail2)
	}
	resumed := controlReply(t, daemon, "/resume 2")
	if !strings.Contains(resumed, "Selected run") || !strings.Contains(resumed, shortTaskID(open[1].ID)) {
		t.Fatalf("/resume 2 resolved the wrong task: %s", resumed)
	}

	// Numbers beyond the open list are a reference mistake, not a 500 and not
	// a bare "Task not found" — done tasks never take a number.
	if out := controlReply(t, daemon, "/task 3"); !strings.Contains(out, "No task number 3") || !strings.Contains(out, "/tasks") {
		t.Fatalf("out-of-range ordinal reply: %s", out)
	}
	if out := controlReply(t, daemon, "/resume 99"); !strings.Contains(out, "No resumable run number 99") {
		t.Fatalf("out-of-range /resume reply: %s", out)
	}
}

// The default open view ranks human-waiting work ahead of newer one-shot
// labels. Ordinal commands must resolve the cards the person actually saw,
// rather than re-sorting the same rows by update time.
func TestTaskOrdinalUsesRankedOpenCardOrder(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	waiting, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "older waiting work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, waiting, "cli", waiting.Title)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: waiting.ID, RunID: run.ID,
		ActionType: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// Task timestamps are stored at second precision; make the ordinary row
	// observably newer so the old updated_at resolver disagrees with QueryTasks.
	time.Sleep(1100 * time.Millisecond)
	newer := seedTask(t, store, identity, "newer ordinary work", "in_progress", 0)

	listed := controlReply(t, daemon, "/tasks")
	if !strings.Contains(listed, "1. [waiting] "+waiting.Title) {
		t.Fatalf("ranked card 1 is not the waiting task:\n%s", listed)
	}
	if detail := controlReply(t, daemon, "/task 1"); !strings.Contains(detail, "ID: "+waiting.ID) {
		t.Fatalf("/task 1 selected %s instead of displayed card 1 %s:\n%s", newer.ID, waiting.ID, detail)
	}
}

func TestTaskOrdinalSnapshotSurvivesLaterReordering(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	first := seedTask(t, store, identity, "snapshot first", "in_progress", 0)
	second := seedTask(t, store, identity, "snapshot second", "in_progress", 0)

	listed := controlReply(t, daemon, "/tasks")
	firstPos := strings.Index(listed, "1. ")
	firstTitle := first.Title
	firstID := first.ID
	if strings.Index(listed, second.Title) > firstPos && strings.Index(listed, second.Title) < strings.Index(listed, first.Title) {
		firstTitle, firstID = second.Title, second.ID
	}

	// Mutating the other task changes the live ranking, but must not change what
	// the already displayed number means on this endpoint.
	otherID := first.ID
	if otherID == firstID {
		otherID = second.ID
	}
	if err := store.SetTaskPinned(context.Background(), identity.TenantID, otherID, true); err != nil {
		t.Fatal(err)
	}
	if detail := controlReply(t, daemon, "/task 1"); !strings.Contains(detail, "ID: "+firstID) {
		t.Fatalf("displayed card 1 (%s) drifted after reordering:\n%s", firstTitle, detail)
	}
}

func TestTaskCompleteAcceptsOrdinalAndIDAndCanBeResumed(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	first := seedTask(t, store, identity, "finish by id", "in_progress", 0)
	second := seedTask(t, store, identity, "finish by number", "in_progress", 0)

	listed := controlReply(t, daemon, "/tasks")
	numbered := first
	byID := second
	if strings.Index(listed, second.Title) < strings.Index(listed, first.Title) {
		numbered, byID = second, first
	}
	if reply := controlReply(t, daemon, "/task 1 complete"); !strings.Contains(reply, "Dismissed current attention") {
		t.Fatalf("ordinal completion reply: %s", reply)
	}
	if reply := controlReply(t, daemon, "/task "+byID.ID+" complete"); !strings.Contains(reply, "Dismissed current attention") {
		t.Fatalf("id completion reply: %s", reply)
	}
	if attention, err := control.NewWorkTimeline(store).Attention(context.Background(), identity.TenantID, identity.PersonID, 10); err != nil || len(attention) != 0 {
		t.Fatalf("dismissed work remains in Attention: %+v err=%v", attention, err)
	}
	if reply := controlReply(t, daemon, "/resume "+numbered.ID); !strings.Contains(reply, "Selected run") {
		t.Fatalf("explicit resume did not accept completed task: %s", reply)
	}
}

// TestResumeAcceptsCardDisplayedID: the exact shortened id printed on a /tasks
// card (cardTaskID) must round-trip through /resume — the card is where users
// copy the reference from (observed live: /resume task_68a72a44 → "Task not
// found." because only the full uuid was accepted).
func TestResumeAcceptsCardDisplayedID(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "resume by card id", "in_progress", 1)

	display := cardTaskID(task.ID)
	out := controlReply(t, daemon, "/resume "+display)
	if !strings.Contains(out, "Selected run") || !strings.Contains(out, shortTaskID(task.ID)) {
		t.Fatalf("card id %q did not resume the task: %s", display, out)
	}
	// The shortTaskID hint form (task_ + 8 chars) resolves too.
	out = controlReply(t, daemon, "/resume "+shortTaskID(task.ID))
	if !strings.Contains(out, "Selected run") {
		t.Fatalf("short id did not resume: %s", out)
	}
}

// TestResumeReplyStatesWorkspaceBinding: the /resume success reply names the
// task's bound workspace (or says none is bound). The status bar keeps showing
// the launch cwd until the next turn, so without this line a working /resume
// reads as broken (observed live).
func TestResumeReplyStatesWorkspaceBinding(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	path := t.TempDir()
	ws, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "game", LocalPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound := seedTask(t, store, identity, "bound to game", "in_progress", 1)
	if err := store.SetTaskWorkspace(ctx, identity.TenantID, bound.ID, ws.ID); err != nil {
		t.Fatal(err)
	}
	unbound := seedTask(t, store, identity, "no workspace", "in_progress", 1)

	out := controlReply(t, daemon, "/resume "+bound.ID)
	if !strings.Contains(out, "workspace: game ("+path+"); your next message runs there.") {
		t.Fatalf("bound workspace note missing: %s", out)
	}
	out = controlReply(t, daemon, "/resume "+unbound.ID)
	if !strings.Contains(out, "no workspace bound; your next message uses the current workspace.") {
		t.Fatalf("unbound workspace note missing: %s", out)
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
	if !strings.Contains(runsOut, "[done]") || !strings.Contains(runsOut, "[waiting_user]") {
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
	if !strings.Contains(archived, "Archived thread") {
		t.Fatalf("archive reply: %s", archived)
	}
	got, _ = store.GetTask(context.Background(), identity.TenantID, task.ID)
	if got.Visibility != control.ThreadVisibilityArchived {
		t.Fatalf("visibility = %q", got.Visibility)
	}

	// Usage guards.
	if out := controlReply(t, daemon, "/task"); !strings.Contains(out, "Usage: /task") {
		t.Fatalf("bare /task should print usage: %s", out)
	}
	if out := controlReply(t, daemon, "/task task_nope-nope"); !strings.Contains(out, "not found") {
		t.Fatalf("missing task should say not found: %s", out)
	}
}

func TestTaskReferenceCommandsRoundTrip(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Reference command work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	short := shortTaskID(task.ID)
	if out := controlReply(t, daemon, "/task "+short+" reference add customer portal"); !strings.Contains(out, "Added task reference") {
		t.Fatalf("add output=%q", out)
	}
	if out := controlReply(t, daemon, "/task "+short+" references"); !strings.Contains(out, "customer portal") || !strings.Contains(out, "active") {
		t.Fatalf("list output=%q", out)
	}
	if out := controlReply(t, daemon, "/task "+short+" reference remove customer portal"); !strings.Contains(out, "Removed task reference") {
		t.Fatalf("remove output=%q", out)
	}
}

func TestTaskPinCommandsAndCard(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "important long-running work", "in_progress", 1)

	if out := controlReply(t, daemon, "/task "+task.ID+" pin"); !strings.Contains(out, "Pinned task") {
		t.Fatalf("pin reply: %s", out)
	}
	got, _ := store.GetTask(context.Background(), identity.TenantID, task.ID)
	if got == nil || !got.Pinned {
		t.Fatalf("task was not pinned: %+v", got)
	}
	if out := controlReply(t, daemon, "/tasks"); !strings.Contains(out, "pinned: yes") {
		t.Fatalf("pin state missing from card:\n%s", out)
	}
	seedTask(t, store, identity, "newer ordinary work", "in_progress", 1)
	if out := controlReply(t, daemon, "/tasks"); strings.Index(out, task.Title) > strings.Index(out, "newer ordinary work") {
		t.Fatalf("pinned task must remain before newer ordinary work:\n%s", out)
	}
	if out := controlReply(t, daemon, "/task "+task.ID+" unpin"); !strings.Contains(out, "Unpinned task") {
		t.Fatalf("unpin reply: %s", out)
	}
	got, _ = store.GetTask(context.Background(), identity.TenantID, task.ID)
	if got == nil || got.Pinned {
		t.Fatalf("task was not unpinned: %+v", got)
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
	// Implicit continuation never resurrects archived work: the run ladder
	// excludes runs whose task is archived.
	candidates, err := store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range candidates {
		if run.TaskID == task.ID {
			t.Fatalf("continuation ladder offered an archived label's run: %+v", run)
		}
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

// TestTasksPagesBeyondOneHundredAttentionItems: /tasks requests the page it
// draws, so work past the first page is reachable. The first page's hint
// counts everything that needs attention, the second page renders the exact
// runs the ranking names, and its ordinals resolve to those runs.
func TestTasksPagesBeyondOneHundredAttentionItems(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	const parked = 105
	for i := 0; i < parked; i++ {
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			Title: fmt.Sprintf("parked release %03d", i), Channel: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", task.Title)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
	}
	// The ranked order is the timeline's, not the seeding order: same-second
	// runs tie-break by run id. Ask for it once and hold every page to it.
	ranked, total, err := control.NewWorkTimeline(store).AttentionPage(ctx, identity.TenantID, identity.PersonID, "cli", parked+10, 0)
	if err != nil || total != parked || len(ranked) != parked {
		t.Fatalf("timeline attention=%d total=%d err=%v, want %d parked runs", len(ranked), total, err, parked)
	}

	first := controlReply(t, daemon, "/tasks open --limit 100")
	if !strings.Contains(first, fmt.Sprintf("... and %d more - use /tasks open --page 2", parked-100)) {
		t.Fatalf("page one must count every attention item, not the fetched page:\n%s", tailLines(first, 6))
	}
	second := controlReply(t, daemon, "/tasks open --limit 100 --page 2")
	for i, item := range ranked[100:] {
		if !strings.Contains(second, cardTaskID(item.Thread.ID)) {
			t.Fatalf("page two is missing attention item %d (%s):\n%s", 101+i, item.Thread.ID, tailLines(second, 12))
		}
		if !strings.Contains(second, fmt.Sprintf("%d. [", i+1)) {
			t.Fatalf("page two card %d is unnumbered, so its ordinal cannot resolve:\n%s", i+1, tailLines(second, 12))
		}
	}
	if strings.Contains(second, cardTaskID(ranked[0].Thread.ID)) {
		t.Fatalf("page two repeated page one's first card:\n%s", tailLines(second, 12))
	}
	if strings.Contains(second, "... and ") {
		t.Fatalf("the last page must not claim more pages:\n%s", tailLines(second, 6))
	}

	// The ordinal belongs to the page that was drawn: 1 on page two is the
	// 101st attention item, not the first.
	selected := controlReply(t, daemon, "/resume 1")
	if !strings.Contains(selected, "Selected run "+shortRunID(ranked[100].RunID)) {
		t.Fatalf("page two ordinal resolved to the wrong run: %s (want %s)", selected, shortRunID(ranked[100].RunID))
	}
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
