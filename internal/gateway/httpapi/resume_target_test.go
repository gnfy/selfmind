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
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
)

// seedUnresolvedRun starts one run and parks it in a resumable status so it
// counts as an unclaimed continuation parent.
func seedUnresolvedRun(t *testing.T, store *control.Store, tenantID string, task *control.Task, channel, input, status string) *control.Run {
	t.Helper()
	run, err := store.StartRun(context.Background(), task, channel, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(context.Background(), tenantID, run.ID, status); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestAmbiguousContinuationReturnsCandidatesWithoutModel pins the P0
// acceptance row: two unfinished runs under one task and a vague "继续" must
// produce a deterministic candidate list — no model run, no new task_runs row.
func TestAmbiguousContinuationReturnsCandidatesWithoutModel(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "gcp release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "gcp 生产发布，先不要执行", "waiting_user")
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "开始执行", "interrupted")

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
	if !strings.Contains(resp.Content, "several possible continuations") || resp.Choice == nil {
		t.Fatalf("expected deterministic candidate list, got: %+v", resp)
	}
	if resp.Turn == nil || resp.Turn.Status != "waiting_user" || resp.Turn.RunID != "" {
		t.Fatalf("candidate turn must not carry a run: %+v", resp.Turn)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("no run may start on an ambiguous continuation, got %d runs", len(runs))
	}
}

// TestDaemonOriginCueNeverSteersActiveRun pins the P0 origin gate: a turn the
// daemon itself originated (cron/watch/approval) whose text happens to contain
// a continuation cue must queue durably, never steer the person's active run.
func TestDaemonOriginCueNeverSteersActiveRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	coord := daemon.coordinator()
	if ok := coord.beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli", Summary: "long build", StartedAt: time.Now(),
		Steer: make(chan kernel.SteeringInput, 1),
	}); !ok {
		t.Fatal("failed to register the active run")
	}
	defer coord.endActive(identity.PersonID)

	cron, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cron", Content: "继续检查部署", Origin: "cron",
	})
	if status != http.StatusOK || cron.Turn == nil || cron.Turn.Status != "queued" {
		t.Fatalf("cron-origin cue must queue behind the active run, got status=%d turn=%+v", status, cron.Turn)
	}

	user, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if status != http.StatusOK || user.Turn == nil || user.Turn.Status == "queued" {
		t.Fatalf("user-origin cue must still reach the steer path, got status=%d turn=%+v", status, user.Turn)
	}
}

// TestReplyMetadataBindsExactRun pins the P1 structured reply edge: a request
// carrying reply_to_run_id binds its task AND its exact parent run, immune to
// the current-task pointer and to sibling unresolved runs.
func TestReplyMetadataBindsExactRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	replyTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "old release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := seedUnresolvedRun(t, store, identity.TenantID, replyTask, "cli", "prepare the release", "waiting_user")
	seedUnresolvedRun(t, store, identity.TenantID, replyTask, "cli", "sibling waiting work", "interrupted")
	// The current-task pointer deliberately points somewhere else.
	if _, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "unrelated current", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "好的，开始执行", ReplyToRunID: target.ID,
	})
	if resp.Task == nil || resp.Task.ID != replyTask.ID {
		t.Fatalf("reply must bind the run's own task, got %+v", resp.Task)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, replyTask.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var child *control.Run
	for i := range runs {
		if runs[i].ResumesRunID == target.ID {
			child = &runs[i]
		}
	}
	if child == nil {
		t.Fatalf("reply continuation must claim exactly the named parent, runs=%+v", runs)
	}
}

func TestStructuredReturnEdgesFailClosed(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: other.TenantID, PersonID: other.PersonID, Title: "private work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRun := seedUnresolvedRun(t, store, other.TenantID, otherTask, "cli", "private wait", "waiting_user")
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "apply this", ReplyToRunID: otherRun.ID,
	})
	if status != http.StatusConflict || !strings.Contains(resp.Error, "reply target is invalid") {
		t.Fatalf("foreign reply edge must fail closed: status=%d resp=%+v", status, resp)
	}

	ownerTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "approval work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerRun := seedUnresolvedRun(t, store, owner.TenantID, ownerTask, "cli", "approval wait", "waiting_user")
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: ownerTask.ID,
		RunID: ownerRun.ID, ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "run it", ApprovalID: approval.ID,
	})
	if status != http.StatusConflict || !strings.Contains(resp.Error, "approval continuation is invalid") {
		t.Fatalf("pending approval edge must fail closed: status=%d resp=%+v", status, resp)
	}
}

func TestExplicitTaskIDClaimsUniqueParent(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "explicit continuation", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "waiting for a choice", "waiting_user")

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	_, _ = daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		TaskID: task.ID, Content: "use the first option",
	})
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ResumesRunID == parent.ID {
			return
		}
	}
	t.Fatalf("explicit task continuation did not claim parent %s: %+v", parent.ID, runs)
}

func TestExplicitTaskIDAmbiguityDoesNotStartRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "ambiguous explicit continuation", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "line one", "waiting_user")
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "line two", "interrupted")

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		TaskID: task.ID, Content: "use the selected line",
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "several possible continuations") || resp.Choice == nil {
		t.Fatalf("explicit ambiguity must return candidates: status=%d resp=%+v", status, resp)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("ambiguous explicit continuation started a run: len=%d err=%v", len(runs), err)
	}
}

func TestResumeRunReferenceClaimsExactParent(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "pick exact run", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "target line", "waiting_user")
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "other line", "interrupted")
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	selected, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/resume " + target.ID,
	})
	if status != http.StatusOK || !strings.Contains(selected.Content, "Selected run") {
		t.Fatalf("run selection failed: status=%d resp=%+v", status, selected)
	}
	_, _ = daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "apply that decision",
	})
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ResumesRunID == target.ID {
			return
		}
	}
	t.Fatalf("selected run was not claimed as parent: %+v", runs)
}

func TestReplyMetadataSteersNamedActiveRunWithoutCue(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "active reply", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "choose a region")
	if err != nil {
		t.Fatal(err)
	}
	steer := make(chan kernel.SteeringInput, 1)
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "cli", Platform: "cli", Steer: steer,
	}) {
		t.Fatal("failed to seed active run")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "us-east-1", ReplyToRunID: run.ID,
	})
	if status != http.StatusOK || !resp.Accepted || resp.Turn == nil || resp.Turn.Status != "accepted" {
		t.Fatalf("exact active reply must steer: status=%d resp=%+v", status, resp)
	}
	select {
	case got := <-steer:
		if got.Content != "us-east-1" {
			t.Fatalf("steered content=%q", got.Content)
		}
	default:
		t.Fatal("exact reply was not delivered to the active steering channel")
	}
}

// TestApprovalReturnBindsOriginRun pins the P1 structured approval edge: a
// daemon-originated approval continuation reaches the parked approval's origin
// run exactly — even with sibling unresolved runs — with no prose parsing.
func TestApprovalReturnBindsOriginRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "gcp release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "waiting for approval", "waiting_user")
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "another parked line", "interrupted")
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: origin.ID, ActionType: "terminal", Status: "approved",
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content:    "The pending action was approved.",
		Origin:     "approval",
		ApprovalID: approval.ID,
	})
	if resp.Task == nil || resp.Task.ID != task.ID {
		t.Fatalf("approval return must bind the approval's task, got %+v", resp.Task)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var child *control.Run
	for i := range runs {
		if runs[i].ResumesRunID == origin.ID {
			child = &runs[i]
		}
	}
	if child == nil {
		t.Fatalf("approval continuation must claim exactly the origin run, runs=%+v", runs)
	}
}

// TestLoopCheckpointResumeIsParentScoped pins the P0 checkpoint gate: only the
// resolved parent run's incomplete checkpoint may replace the message array —
// a newer checkpoint under a sibling run must not be restored, and no parent
// means no restore at all.
func TestLoopCheckpointResumeIsParentScoped(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "two lines", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "line A", "interrupted")
	sibling := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "line B", "interrupted")
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: sibling.ID,
		Iteration: 3, Outcome: "continue", Snapshot: []byte(`[{"role":"user","content":"line B state"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	intent := router.IntentResult{Intent: router.IntentContinue}

	if got := daemon.coordinator().withLoopCheckpointResume(ctx, identity, task, nil, intent); kernel.HasLoopResumeMessages(got) {
		t.Fatal("no parent must mean no checkpoint restore")
	}
	if got := daemon.coordinator().withLoopCheckpointResume(ctx, identity, task, parent, intent); kernel.HasLoopResumeMessages(got) {
		t.Fatal("a sibling run's checkpoint must not restore under a different parent")
	}
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: parent.ID,
		Iteration: 2, Outcome: "continue", Snapshot: []byte(`[{"role":"user","content":"line A state"}]`),
	}); err != nil {
		t.Fatal(err)
	}
	if got := daemon.coordinator().withLoopCheckpointResume(ctx, identity, task, parent, intent); !kernel.HasLoopResumeMessages(got) {
		t.Fatal("the parent run's own incomplete checkpoint must restore")
	}
	if got := daemon.coordinator().withLoopCheckpointResume(ctx, identity, task, parent, router.IntentResult{Intent: router.IntentTask}); !kernel.HasLoopResumeMessages(got) {
		t.Fatal("an exact structured parent must restore regardless of natural-language intent")
	}
}
