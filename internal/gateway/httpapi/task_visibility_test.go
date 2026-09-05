package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

// TestAsyncRunOwnsFreshInteractionThread covers the no-current-pointer model:
// an ordinary async send owns a fresh root and cannot capture unrelated work.
func TestAsyncRunOwnsFreshInteractionThread(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)

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
	// A second open label keeps the attachment genuinely ambiguous. Without a
	// post-run analyzer the guessed current label must retain its lifecycle.
	if _, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Other open work", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
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

	// The run executes in the background on its OWN fresh task (P2): the
	// unrelated current label keeps its lifecycle untouched, and the pointer
	// (a UI projection) follows the task the run actually resolved.
	var freshID string
	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, err := control.NewWorkTimeline(store).Search(ctx, identity.TenantID, identity.PersonID, "background refactor", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, thread := range matches {
			if thread.ID != currentTask.ID {
				freshID = thread.ID
			}
		}
		if freshID != "" {
			runs, err := store.ListTaskRuns(ctx, identity.TenantID, freshID, 10)
			if err == nil && len(runs) == 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("async Run never appeared under fresh Thread %s", freshID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := store.GetTask(ctx, identity.TenantID, currentTask.ID)
	if err != nil || after == nil || after.Title != currentTask.Title {
		t.Fatalf("the unrelated current label must stay untouched: %+v err=%v", after, err)
	}
	if runs, err := store.ListTaskRuns(ctx, identity.TenantID, currentTask.ID, 10); err != nil || len(runs) != 0 {
		t.Fatalf("the unrelated Thread captured execution: %+v err=%v", runs, err)
	}
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current != nil && current.ID == currentTask.ID {
		t.Fatalf("unrelated work became an implicit continuation authority: %+v", current)
	}
}

// The continuation ladder binds the sole waiting run; without any analyzer
// the finalize path itself must commit the derived lifecycle onto the task.
func TestSoleWaitingRunContinuationReconcilesWithoutAnalyzer(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Only open work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.StartRun(ctx, task, "cli", "prepare the only open work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, waiting.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "send", Content: "continue", Async: true,
	})
	if status != http.StatusOK || !resp.Accepted {
		t.Fatalf("async accept failed: status=%d resp=%+v", status, resp)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range runs {
			if candidate.ResumesRunID == waiting.ID && candidate.Status == "failed" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sole continuation did not record a failed child Run: %+v", runs)
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
	store := controltest.NewStore(t)

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
	if !strings.Contains(reply, "Task: "+activeTask.Title) {
		t.Fatalf("status after registry release should still derive the running Run: %q", reply)
	}
}

// TestStatusSurfacesPendingApproval: a run blocked on an approval must not
// look "stuck" — /status carries the same conversational y/n prompt the push
// uses (observed live: 15 minutes of staring at a silent "running" card).
func TestStatusSurfacesPendingApproval(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "await approval")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "terminal", Payload: []byte(`{"command":"rm -rf build"}`),
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := daemon.statusReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Waiting for your approval", "elapsed)", "reply y or n", "[terminal]", "rm -rf build"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("status card missing %q:\n%s", want, reply)
		}
	}
}

// TestTaskEventsEndpointDerivesSubjectFromAttention: a subject-less events poll
// after a turn parked follows the top Attention item instead of returning an
// empty list, so a finished turn's activity stays visible to the TUI.
func TestTaskEventsEndpointDerivesSubjectFromAttention(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Parked work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "park after one tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: mustJSON(map[string]string{"tool": "read_file"})}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	subject, err := daemon.subjectThreadID(ctx, identity)
	if err != nil || subject != task.ID {
		t.Fatalf("subject = %q, %v; want attention thread %s", subject, err, task.ID)
	}
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/events?platform=cli&platform_user_id=local", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Task   *control.Task   `json:"task"`
		Events []control.Event `json:"events"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Task == nil || payload.Task.ID != task.ID || len(payload.Events) == 0 {
		t.Fatalf("subject-less events poll = %+v", payload)
	}
	// /diag reads the same subject for its recent-activity section.
	diag, err := daemon.diagReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diag, "Recent activity:") {
		t.Fatalf("/diag lost recent activity after the turn parked:\n%s", diag)
	}

	// With the Attention dismissed, the most recent Run still names the subject.
	if ok, err := control.NewWorkTimeline(store).DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID); err != nil || !ok {
		t.Fatalf("dismiss = %v, %v", ok, err)
	}
	if subject, err := daemon.subjectThreadID(ctx, identity); err != nil || subject != task.ID {
		t.Fatalf("subject after dismissal = %q, %v; want most recent run's thread", subject, err)
	}
}

// Pending input is person-scoped, so an approval or question raised by another
// Thread still belongs on the status card — but it must name its own Thread, or
// it reads as if it came from the work the card describes.
func TestStatusNamesPendingInputFromAnotherThread(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	// The card describes the executing Run of this Thread.
	running, err := store.StartRun(ctx, task, "cli", "current work")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "gcp 生产发布", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRun, err := store.StartRun(ctx, other, "cli", "release gcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, otherRun.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: other.ID, RunID: otherRun.ID,
		ActionType: "terminal", Payload: []byte(`{"tool":"terminal","args":{"command":"gcloud deploy"},"reason":"production release"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: other.ID, RunID: otherRun.ID,
		Question: "which region?",
	}); err != nil {
		t.Fatal(err)
	}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: running.ID, Channel: "cli", StartedAt: time.Now(),
	}) {
		t.Fatal("active run registration failed")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	reply, err := daemon.statusReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gcloud deploy", "which region?", "(task: gcp 生产发布)"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("status card missing %q:\n%s", want, reply)
		}
	}
	if strings.Count(reply, "(task: gcp 生产发布)") != 2 {
		t.Fatalf("both the approval and the question must name their own thread:\n%s", reply)
	}
}
