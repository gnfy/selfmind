package httpapi

// Attention listing and reference resolution. These go through ProcessMessage
// so every endpoint (CLI, IM) gets identical behavior.
//
// This replaces the /tasks card view and the /task subcommands. What the person
// can do is now stated in run terms: `/resume` lists what needs them, a number
// or a run id continues one exactly, `/stop` dismisses one, and `/search` finds
// past work. The tests below keep the behaviors that survived that change and
// pin the ones the removal must NOT have taken with it.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/memory"
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

// advanceRunClock blocks until the wall clock has left the second in which
// `since` was recorded, so the next StartRun sorts strictly after it. Run
// timestamps have second precision, and the Attention "latest Run" rule breaks
// same-second ties by id, which is random.

// TestBareResumeListsAttentionAndPinsTheExactRun is the core of the surface
// that replaced /tasks: the listing names runs, and a number continues one
// EXACTLY. The ordinal snapshot used to live in the /tasks render, which made
// "continue number 2" depend on a listing about labels.
func TestBareResumeListsAttentionAndPinsTheExactRun(t *testing.T) {
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

	listed := controlReply(t, daemon, "/resume")
	for _, want := range []string{"Needs attention", "release waiting for confirmation", shortRunID(run.ID)} {
		if !strings.Contains(listed, want) {
			t.Fatalf("attention list missing %q:\n%s", want, listed)
		}
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

// TestAttentionListKeepsTwoRunsOfOneThreadDistinct: Attention is per exact Run.
// Two parked runs that happen to share a thread are two things the person can
// act on, and collapsing them would hide one.
func TestAttentionListKeepsTwoRunsOfOneThreadDistinct(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	_, older, newer := seedTwoAttentionRunsInOneThread(t, store, identity, "two release confirmations")

	listed := controlReply(t, daemon, "/resume")
	for _, run := range []*control.Run{older, newer} {
		if !strings.Contains(listed, shortRunID(run.ID)) {
			t.Fatalf("attention list dropped run %s:\n%s", shortRunID(run.ID), listed)
		}
	}
}

// TestAttentionOrdinalSnapshotSurvivesReordering: the numbers belong to the
// list the person actually saw. New work arriving between the listing and the
// choice must not silently redirect "continue number 1".
func TestAttentionOrdinalSnapshotSurvivesReordering(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	first := seedTask(t, store, identity, "first parked work", "waiting_user", 1)
	firstRuns, err := store.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, first.ID, 5)
	if err != nil || len(firstRuns) != 1 {
		t.Fatalf("runs=%+v err=%v", firstRuns, err)
	}
	listed := controlReply(t, daemon, "/resume")
	if !strings.Contains(listed, shortRunID(firstRuns[0].ID)) {
		t.Fatalf("listing missing the seeded run:\n%s", listed)
	}

	advanceRunClock(firstRuns[0].StartedAt)
	seedTask(t, store, identity, "newer parked work", "waiting_user", 1)

	selected := controlReply(t, daemon, "/resume 1")
	if !strings.Contains(selected, "Selected run "+shortRunID(firstRuns[0].ID)) {
		t.Fatalf("ordinal resolved against a list the person never saw: %s", selected)
	}
}

// TestAttentionListNamesWhyAnInterruptedRunStopped keeps the one piece of card
// detail worth keeping: "interrupted" alone does not tell anyone whether the
// work is worth continuing.
func TestAttentionListNamesWhyAnInterruptedRunStopped(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	task := seedTask(t, store, identity, "interrupted deploy", "interrupted", 1)
	runs, err := store.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, task.ID, 5)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	// The outcome is read from the durable run.interrupted event, which is how
	// the daemon records why a run stopped.
	if _, err := store.AppendEvent(ctx, control.Event{
		TenantID: identity.TenantID, TaskID: task.ID, RunID: runs[0].ID,
		Type: "run.interrupted", Visibility: "task",
		Payload: mustJSON(map[string]any{"outcome": map[string]any{
			"completion_reason": "daemon_recovery", "resumable": true,
		}}),
	}); err != nil {
		t.Fatal(err)
	}
	listed := controlReply(t, daemon, "/resume")
	if !strings.Contains(listed, "daemon restarted") {
		t.Fatalf("interrupted reason missing from the listing:\n%s", listed)
	}
}

// TestRetiredTaskCommandsAreGone: the commands must not linger as silent
// no-ops. An unknown command is a clear answer; a command that quietly does
// nothing is not.
func TestRetiredTaskCommandsAreGone(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedTask(t, store, identity, "some work", "waiting_user", 1)
	for _, retired := range []string{"/tasks", "/tasks done", "/task 1", "/diag tasks"} {
		resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: retired,
		})
		if status == http.StatusOK && strings.Contains(resp.Content, "Needs attention:") {
			t.Fatalf("%q still renders the retired listing: %s", retired, resp.Content)
		}
	}
}

// TestSearchFindsPastWorkFromAnyEndpoint pins the retrieval replacement. The
// keyword search used to live under /tasks (gateway) while /search was
// terminal-only, so deleting /tasks without this would have removed history
// search from IM entirely.
func TestSearchFindsPastWorkFromAnyEndpoint(t *testing.T) {
	daemon, _, _ := newTaskViewServer(t)
	daemon.Sessions = &stubSessionsBackend{}
	out := controlReply(t, daemon, "/search aurora gate")
	if !strings.Contains(out, "aurora gate release") {
		t.Fatalf("search reply did not surface the match:\n%s", out)
	}
	if bare := controlReply(t, daemon, "/search"); !strings.Contains(bare, "Recent working sessions") {
		t.Fatalf("bare search must list recent sessions:\n%s", bare)
	}
}

// stubSessionsBackend is a fixed session index: /search must render what the
// index returns without needing a live memory store.
type stubSessionsBackend struct{}

func (stubSessionsBackend) SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error) {
	return []memory.FTS5Session{{
		SessionID: "run:run_prior", Channel: "cli",
		Summary: "promoted the aurora gate release", Timestamp: 1,
	}}, nil
}

func (stubSessionsBackend) ListRecentSessions(tenantID string, limit int) ([]memory.FTS5Session, error) {
	return []memory.FTS5Session{{
		SessionID: "run:run_recent", Channel: "cli",
		Summary: "most recent working session", Timestamp: 2,
	}}, nil
}

func (stubSessionsBackend) GetSessionMessages(tenantID, sessionID string, aroundMessageID, window int) ([]memory.SessionMessage, error) {
	return nil, nil
}

// TestListingsHideMeaninglessChannelIdentifiers: the terminal mints a fresh
// UUID per launch and stores it as the channel. Printing it put an identifier
// nobody can read on every line and pushed the summary off the end.
func TestListingsHideMeaninglessChannelIdentifiers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"cli", "cli"},
		{"weixin", "weixin"},
		{"79620b1b-baf0-4e18-a42b-9fb9467fffcb", ""},
		{"  f78e99f2-6afb-4bab-bc3f-8c4362cb058d  ", ""},
		{"79620b1b-baf0-4e18-a42b-9fb9467ffff", "79620b1b-baf0-4e18-a42b-9fb9467ffff"},
		{"", ""},
	} {
		if got := displayChannel(tc.in); got != tc.want {
			t.Errorf("displayChannel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
