package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
)

type flakyRecordingSender struct {
	messages []delivery.Message
	err      error
}

func (s *flakyRecordingSender) Send(_ context.Context, msg delivery.Message) error {
	if s.err != nil {
		return s.err
	}
	s.messages = append(s.messages, msg)
	return nil
}

func TestRecoveryNotificationDeliveredOnce(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "long-running deployment")
	if err != nil {
		t.Fatal(err)
	}
	// A specialist-owned approval Run keeps the compatibility notification
	// path; generic automatic recovery must not steal its row-specific resume.
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, ActionType: "tool_call", RequestedChannel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_recovery", "WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	if recovered, err := store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("recover: count=%d err=%v", recovered, err)
	}

	daemon.sweepRecoveryNotifications()
	if len(recorder.messages) != 1 {
		t.Fatalf("messages = %+v", recorder.messages)
	}
	msg := recorder.messages[0]
	if msg.Kind != "recovery" || msg.RunID != run.ID || msg.Platform != "weixin" {
		t.Fatalf("recovery message = %+v", msg)
	}
	if !strings.Contains(msg.Content, "Recovery handoff:") ||
		!strings.Contains(msg.Content, "the exact approval flow owns this continuation") ||
		!strings.Contains(msg.Content, "/resume "+run.ID) ||
		strings.Contains(msg.Content, "safe and resumable") {
		t.Fatalf("content = %q", msg.Content)
	}

	daemon.sweepRecoveryNotifications()
	if len(recorder.messages) != 1 {
		t.Fatalf("recovery notification was delivered twice: %+v", recorder.messages)
	}
}

func TestAutomaticRecoveryDisableFallsBackToStructuredHandoff(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "inspect and continue safely")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_recovery_disabled", "WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.DisableAutomaticRunRecovery = true
	if recovered, err := store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("recover: count=%d err=%v", recovered, err)
	}

	daemon.sweepRecoveryNotifications()
	if len(recorder.messages) != 1 {
		t.Fatalf("messages=%+v", recorder.messages)
	}
	content := recorder.messages[0].Content
	if !strings.Contains(content, "automatic continuation is disabled") ||
		!strings.Contains(content, "Original goal: inspect and continue safely") ||
		!strings.Contains(content, "/resume "+run.ID) {
		t.Fatalf("content=%q", content)
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(queued) != 0 {
		t.Fatalf("disabled recovery queued work: %+v err=%v", queued, err)
	}
}

func TestAutomaticRecoveryQueuesOneExactParentBelowForeground(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "inspect and continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_auto_recovery", "WeChat"); err != nil {
		t.Fatal(err)
	}
	if recovered, err := store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("recover: count=%d err=%v", recovered, err)
	}
	items, err := store.ListPendingRecoveryNotifications(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	for i := 0; i < 2; i++ {
		scheduled, err := daemon.scheduleAutomaticRunRecovery(ctx, items[0], false)
		if err != nil || !scheduled {
			t.Fatalf("schedule %d: scheduled=%v err=%v", i, scheduled, err)
		}
	}
	rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	queued := rows[0]
	if queued.TaskID != task.ID || queued.ReplyToRunID != run.ID || queued.Class != control.QueueClassRecovery ||
		queued.Priority >= control.QueuePriorityForeground || queued.Priority <= control.QueuePriorityBackground {
		t.Fatalf("queued recovery=%+v", queued)
	}
	if got := recoveryModeFromQueueKey(queued.IdempotencyKey); got != control.RunRecoveryModeContinue {
		t.Fatalf("recovery mode=%q", got)
	}
	items, err = store.ListPendingRecoveryNotifications(ctx, 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("scheduled recovery still requested a fallback notification: %+v err=%v", items, err)
	}
}

func TestAutomaticRecoveryDisableCancelsPreviouslyScheduledRowBeforeClaim(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "continue after restart")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_recovery_rollback", "WeChat"); err != nil {
		t.Fatal(err)
	}
	if recovered, err := store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("recover: count=%d err=%v", recovered, err)
	}
	items, err := store.ListPendingRecoveryNotifications(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if scheduled, err := daemon.scheduleAutomaticRunRecovery(ctx, items[0], false); err != nil || !scheduled {
		t.Fatalf("schedule=%v err=%v", scheduled, err)
	}
	rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queued=%+v err=%v", rows, err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.DisableAutomaticRunRecovery = true
	if !daemon.cancelDisabledRecoveryQueue(ctx, identity, &rows[0]) {
		t.Fatal("disabled recovery row was not handled")
	}
	if queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || len(queued) != 0 {
		t.Fatalf("recovery row remained claimable: %+v err=%v", queued, err)
	}
	if cancelled, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusCancelled); err != nil || len(cancelled) != 1 {
		t.Fatalf("cancelled=%+v err=%v", cancelled, err)
	}
	if len(recorder.messages) != 1 || !strings.Contains(recorder.messages[0].Content, "automatic continuation is disabled") ||
		!strings.Contains(recorder.messages[0].Content, "/resume "+run.ID) {
		t.Fatalf("messages=%+v", recorder.messages)
	}
}

func TestExternalWatchCompletesOutsideAgentRun(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()
	// This scenario verifies watcher finalization, not approval parking. Use a
	// clean task so scheduler timing cannot race the fixture's unrelated pending
	// approval into waiting_user.
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Monitor external build",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_watch", "WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	// The registering Run hands off as waiting_external; the watcher owns the
	// wait from here.
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	// Another person-level run keeps the drained finalization queued, so the
	// durable products can be asserted without a model.
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "busy"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)
	command := "printf READY"
	if runtime.GOOS == "windows" {
		command = "Write-Output READY"
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		TaskID:                task.ID,
		RunID:                 run.ID,
		Channel:               "cli",
		Description:           "CI build",
		CWD:                   t.TempDir(),
		Command:               command,
		SuccessPattern:        "READY",
		IntervalSeconds:       5,
		CommandTimeoutSeconds: 10,
		TimeoutAt:             time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	// While the watcher is live, the Run is monitoring Attention and nothing
	// may claim it away from the watcher.
	if attention, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10); err != nil ||
		len(attention) != 1 || attention[0].RunID != run.ID || attention[0].Activity != control.ThreadActivityMonitoring {
		t.Fatalf("attention while watching = %+v, %v", attention, err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "too early", control.StartRunOptions{ParentRunID: run.ID}); !errors.Is(err, control.ErrParentRunNotResumable) {
		t.Fatalf("claim during live watch = %v, want ErrParentRunNotResumable", err)
	}

	daemon.runExternalWatchPass(ctx)
	counts, err := store.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID)
	if err != nil || counts[control.ExternalWatchSucceeded] != 1 {
		// The counts alone only say the watch did not succeed. Whether the
		// check ran at all, what it decided, and how many attempts it made are
		// all on the row, and without them a CI-only failure reads as
		// "map[running:1] err=<nil>" and names nothing.
		t.Fatalf("watch did not complete in one pass: counts=%+v err=%v\n%s",
			counts, err, describeWatch(ctx, t, store, identity.TenantID, watch.ID))
	}
	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("watch row = %+v, %v", stored, err)
	}
	// The finalization row replies to the watcher Run, so the drained run
	// becomes that Run's exact child; the Run itself stays parked for it.
	queued, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, externalWatchFinalizationKey(*stored))
	if err != nil || queued == nil || queued.Status != control.QueueStatusQueued || queued.TaskID != task.ID || queued.ReplyToRunID != run.ID {
		t.Fatalf("finalization row = %+v, %v; want queued reply to %s", queued, err, run.ID)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "waiting_external" {
		t.Fatalf("watcher run after verdict = %+v, %v", storedRun, err)
	}
	// A concluded watcher is no longer monitoring Attention: the queued
	// finalization owns the next step, not the person.
	if attention, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10); err != nil || len(attention) != 0 {
		t.Fatalf("attention after verdict = %+v, %v", attention, err)
	}
	if !hasEventOfType(t, store, task.ID, "external_watch.completed") {
		t.Fatal("external watch completion event missing")
	}
	foundNotice := false
	for _, msg := range recorder.messages {
		if msg.Kind != "external_watch" {
			continue
		}
		foundNotice = true
		want := "Watcher " + shortExternalWatchID(watch.ID) + " | status: succeeded | task: waiting_finalization"
		if msg.Content != want {
			t.Fatalf("unexpected external watch notice: %q", msg.Content)
		}
	}
	if !foundNotice {
		t.Fatalf("external watch completion notice missing: %+v", recorder.messages)
	}
}

func TestExternalWatchNotificationWaitsForConfirmedEndpointDelivery(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_watch_retry", "WeChat"); err != nil {
		t.Fatal(err)
	}
	sender := &flakyRecordingSender{err: errors.New("temporary endpoint failure")}
	daemon.Delivery = delivery.NewService(store, sender, delivery.Options{RetryBaseDelay: time.Millisecond})
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	command := "printf READY"
	if runtime.GOOS == "windows" {
		command = "Write-Output READY"
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "cli", Description: "CI build", CWD: t.TempDir(), Command: command,
		SuccessPattern: "READY", IntervalSeconds: 5, CommandTimeoutSeconds: 10,
		TimeoutAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon.runExternalWatchPass(ctx)
	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("watch=%+v err=%v", stored, err)
	}
	if !stored.Finalized || stored.Notified {
		t.Fatalf("failed endpoint must preserve retryable notification: %+v", stored)
	}

	sender.err = nil
	time.Sleep(5 * time.Millisecond)
	daemon.runExternalWatchNotificationPass(ctx)
	stored, err = store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil || !stored.Notified {
		t.Fatalf("watch was not marked after confirmed retry: %+v err=%v", stored, err)
	}
	externalNotices := 0
	for _, msg := range sender.messages {
		if msg.Kind == "external_watch" {
			externalNotices++
		}
	}
	if externalNotices != 1 {
		t.Fatalf("confirmed external watch notices=%d messages=%+v", externalNotices, sender.messages)
	}
}

func TestExternalWatchNotificationUsesDurableEventForAttachedCLI(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.presenceTracker().Touch(identity.PersonID, "cli")
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	command := "printf READY"
	if runtime.GOOS == "windows" {
		command = "Write-Output READY"
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "cli", Description: "CI build", CWD: t.TempDir(), Command: command,
		SuccessPattern: "READY", IntervalSeconds: 5, CommandTimeoutSeconds: 10,
		TimeoutAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon.runExternalWatchPass(ctx)
	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil || !stored.Notified {
		t.Fatalf("attached CLI did not acknowledge durable watch event: %+v err=%v", stored, err)
	}
	if !hasEventOfType(t, store, task.ID, "external_watch.completed") {
		t.Fatal("durable CLI completion event missing")
	}
	if len(recorder.messages) != 0 {
		t.Fatalf("attached CLI should suppress duplicate endpoint push: %+v", recorder.messages)
	}
}

func TestExternalWatchFinalizationUsesRecordedEvidence(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "busy"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch := control.ExternalWatch{
		ID: "watch_evidence", TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, Channel: "cli-session", Description: "CI build",
		Status: control.ExternalWatchSucceeded, VerdictRevision: 1, LastOutput: "SUCCESS\n",
	}
	if err := daemon.enqueueExternalWatchFinalization(ctx, watch, identity, "CI build completed."); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetQueuedByIdempotencyKey(ctx, watch.TenantID, externalWatchFinalizationKey(watch))
	if err != nil || row == nil {
		t.Fatalf("queue row = %+v, %v", row, err)
	}
	if strings.Contains(strings.ToLower(row.Content), "re-check") || !strings.Contains(row.Content, "authoritative evidence") || !strings.Contains(row.Content, "SUCCESS") {
		t.Fatalf("finalization prompt does not preserve watcher authority: %q", row.Content)
	}
	if !strings.Contains(row.Content, "unattended finalization") || !strings.Contains(row.Content, "do not invoke terminal") {
		t.Fatalf("finalization prompt lacks unattended execution contract: %q", row.Content)
	}
}

// seedConcludedWatch parks a watcher Run as waiting_external, registers a watch
// for it, and records a succeeded verdict. It returns the reloaded watch row
// (with its verdict revision) and the Run.
func seedConcludedWatch(t *testing.T, store *control.Store, identity *control.IdentityContext, task *control.Task) (*control.ExternalWatch, *control.Run) {
	t.Helper()
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "cli-session", Description: "CI build", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish watch = %v, %v", finished, err)
	}
	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("watch row = %+v, %v", stored, err)
	}
	return stored, run
}

// assertWatcherRunState reads the watcher Run's execution truth and whether
// Attention lists it; watch tests assert this state, never summary prose.
func assertWatcherRunState(t *testing.T, store *control.Store, identity *control.IdentityContext, runID, wantStatus, wantActivity string) {
	t.Helper()
	ctx := context.Background()
	run, err := store.GetRun(ctx, identity.TenantID, runID)
	if err != nil || run == nil || run.Status != wantStatus {
		t.Fatalf("watcher run = %+v, %v; want status %s", run, err, wantStatus)
	}
	attention, err := control.NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		t.Fatal(err)
	}
	gotActivity := ""
	for _, item := range attention {
		if item.RunID == runID {
			gotActivity = item.Activity
		}
	}
	if gotActivity != wantActivity {
		t.Fatalf("attention activity for %s = %q, want %q (attention=%+v)", runID, gotActivity, wantActivity, attention)
	}
}

func TestExternalWatchFinalizationReconcilesDoneQueue(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "rejected", "cli", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, run := seedConcludedWatch(t, store, identity, task)
	row, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: watch.TenantID, PersonID: watch.PersonID, Channel: watch.Channel,
		Platform: "cli", Content: "old finalization", TaskID: watch.TaskID,
		IdempotencyKey: externalWatchFinalizationKey(*watch),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, watch.TenantID, row.ID, "run_incomplete_finalization"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateThreadSummary(ctx, watch.TenantID, watch.TaskID, "old status", nil); err != nil {
		t.Fatal(err)
	}

	daemon.reconcileExternalWatchFinalizations(ctx)
	row, err = store.GetQueued(ctx, watch.TenantID, row.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusQueued || row.Restarts != 1 {
		t.Fatalf("reconciled queue row = %+v, %v", row, err)
	}
	if strings.Contains(row.Content, "old finalization") || !strings.Contains(row.Content, "authoritative evidence") || !strings.Contains(row.Content, "SUCCESS") {
		t.Fatalf("reconciled queue kept stale instructions: %q", row.Content)
	}
	// A retry is still available, so the watcher Run stays parked for its
	// finalization rather than being parked on the person.
	assertWatcherRunState(t, store, identity, run.ID, "waiting_external", "")
}

func TestExternalWatchFinalizationRepairsLegacyGatewayShutdownCancellation(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "rejected", "cli", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, run := seedConcludedWatch(t, store, identity, task)
	row, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: watch.TenantID, PersonID: watch.PersonID, Channel: watch.Channel,
		Platform: "cli", Content: "old finalization", TaskID: watch.TaskID,
		IdempotencyKey: externalWatchFinalizationKey(*watch),
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyRunID := "run_legacy_shutdown"
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, watch.TenantID, row.ID, legacyRunID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	// The watcher Run's OWN cancellation was a daemon shutdown, which is
	// recoverable, not a decision.
	for _, payload := range []map[string]interface{}{
		{"reason": "gateway shutdown"},
		{"error": "context canceled", "outcome": map[string]interface{}{"status": "cancelled"}},
	} {
		encoded, _ := json.Marshal(payload)
		if _, err := store.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: run.ID, Type: "run.cancelled", Payload: encoded,
		}); err != nil {
			t.Fatal(err)
		}
	}

	daemon.reconcileExternalWatchFinalizations(ctx)
	row, err = store.GetQueued(ctx, watch.TenantID, row.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusQueued {
		t.Fatalf("legacy cancelled queue = %+v, %v; want queued", row, err)
	}
	assertWatcherRunState(t, store, identity, run.ID, "waiting_external", "")
}

func TestExternalWatchFinalizationDoesNotReopenLaterUserCancellation(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, run := seedConcludedWatch(t, store, identity, task)
	row, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: watch.TenantID, PersonID: watch.PersonID, Channel: watch.Channel,
		Platform: "cli", Content: "old finalization", TaskID: watch.TaskID,
		IdempotencyKey: externalWatchFinalizationKey(*watch),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	// The person stopped the watcher Run itself. A newer, unrelated Run in
	// the same Thread was later cut by a daemon shutdown; that must not undo
	// the person's decision the way a newest-cancellation scan would.
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "run.cancelled", Payload: json.RawMessage(`{"reason":"user requested stop"}`),
	}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.StartRun(ctx, task, "cli", "unrelated later work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, unrelated.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: unrelated.ID, Type: "run.cancelled", Payload: json.RawMessage(`{"reason":"gateway shutdown"}`),
	}); err != nil {
		t.Fatal(err)
	}

	daemon.reconcileExternalWatchFinalizations(ctx)
	row, err = store.GetQueued(ctx, watch.TenantID, row.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusDone {
		t.Fatalf("explicitly cancelled queue = %+v, %v; want done", row, err)
	}
	if hasEventOfType(t, store, task.ID, "external_watch.finalization_blocked") {
		t.Fatal("suppressed finalization must not be reported as blocked")
	}
}

// TestExternalWatchFinalizationBindsAndClaimsWatcherRun: the finalization row
// replies to the watcher Run, and once that Run's watchers concluded the
// continuation claims it as the exact parent. After the claim, a binding that
// still names the watcher Run resolves to the Thread's current state instead
// of failing closed, so a retry or a later reply is never unroutable.
func TestExternalWatchFinalizationBindsAndClaimsWatcherRun(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, run := seedConcludedWatch(t, store, identity, task)
	if err := daemon.enqueueExternalWatchFinalization(ctx, *watch, identity, "CI build completed."); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetQueuedByIdempotencyKey(ctx, watch.TenantID, externalWatchFinalizationKey(*watch))
	if err != nil || row == nil || row.ReplyToRunID != run.ID || row.TaskID != task.ID {
		t.Fatalf("finalization row = %+v, %v; want reply to %s", row, err, run.ID)
	}
	resolved, err := daemon.coordinator().resolveExplicitParent(ctx, identity, task, run.ID)
	if err != nil || resolved.exact() == nil || resolved.exact().ID != run.ID {
		t.Fatalf("concluded watcher run resolution = %+v, %v", resolved, err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "finalize the release record", control.StartRunOptions{ParentRunID: run.ID})
	if err != nil || child == nil || child.ParentRunID != run.ID {
		t.Fatalf("finalization child = %+v, %v", child, err)
	}
	resolved, err = daemon.coordinator().resolveExplicitParent(ctx, identity, task, run.ID)
	if err != nil || resolved.exact() != nil || resolved.ambiguous() {
		t.Fatalf("claimed watcher run must resolve to the thread's current state, got %+v, %v", resolved, err)
	}
	// A Run that already moved on is never bound as an exact parent.
	if err := store.FinishRun(ctx, identity.TenantID, child.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if parent := daemon.externalWatchFinalizationParent(ctx, control.ExternalWatch{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: child.ID}); parent != "" {
		t.Fatalf("a done run was offered as finalization parent: %s", parent)
	}
}

// TestExternalWatchFinalizationBlockParksWatcherRun: when the finalization
// cannot be materialized, the watcher Run itself becomes blocked, which is what
// puts it back in front of the person as resumable Attention.
func TestExternalWatchFinalizationBlockParksWatcherRun(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "rejected", "cli", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, run := seedConcludedWatch(t, store, identity, task)
	row, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: watch.TenantID, PersonID: watch.PersonID, Channel: watch.Channel,
		Platform: "cli", Content: "finalization", TaskID: watch.TaskID, ReplyToRunID: run.ID,
		IdempotencyKey: externalWatchFinalizationKey(*watch),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The person dropped the finalization from the queue.
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusCancelled); err != nil {
		t.Fatal(err)
	}
	assertWatcherRunState(t, store, identity, run.ID, "waiting_external", "")

	daemon.reconcileExternalWatchFinalizations(ctx)
	assertWatcherRunState(t, store, identity, run.ID, "blocked", control.ThreadActivityResumable)
	if !hasEventOfType(t, store, task.ID, "external_watch.finalization_blocked") {
		t.Fatal("finalization block event missing")
	}
	// Blocking is idempotent and never rewrites a Run that moved on.
	if changed, err := store.MarkExternalWatchRunBlocked(ctx, identity.TenantID, run.ID, "again"); err != nil || changed {
		t.Fatalf("second block changed=%v err=%v", changed, err)
	}
}

// Regression for the live AWS watcher: the check could not even build its
// sandbox, and the watcher polled that identical failure 65 times until its
// deadline. An environment failure must stop the watch on the FIRST check, and
// it must not be reported as a failure of the external operation.
func TestExternalWatchParksEnvironmentFailureOnFirstCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	// A command whose only output is the sandbox's own state-write denial: the
	// engine classifies it as credential_state_readonly, exactly as the live
	// bwrap failure did.
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		TaskID:                task.ID,
		RunID:                 run.ID,
		Channel:               "cli",
		Description:           "CodeBuild batch",
		CWD:                   t.TempDir(),
		Command:               `echo "Unable to create private file [/home/u/.aws/credentials]: Read-only file system" >&2; exit 1`,
		SuccessPattern:        "SUCCEEDED",
		FailurePattern:        "FAILED",
		IntervalSeconds:       5,
		CommandTimeoutSeconds: 10,
		TimeoutAt:             time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon.runExternalWatchPass(ctx)

	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("watch = %+v err=%v", stored, err)
	}
	if stored.Status != control.ExternalWatchBlocked {
		t.Fatalf("watch status = %q, want blocked_environment", stored.Status)
	}
	if stored.Attempts != 1 {
		t.Fatalf("attempts = %d, want the watch to stop after one check", stored.Attempts)
	}
	reason, ok := watchCheckDefect(stored.LastError)
	if !ok || reason != watchReasonBlockedEnvironment {
		t.Fatalf("parked reason = %q (recognized=%v), want blocked_environment: %q", reason, ok, stored.LastError)
	}
	if !hasEventOfType(t, store, task.ID, "external_watch.blocked") {
		t.Fatal("blocked event missing")
	}
	for _, msg := range recorder.messages {
		if msg.Kind != "external_watch" {
			continue
		}
		if !strings.Contains(msg.Content, "blocked:") || strings.Contains(msg.Content, "status: failed") {
			t.Fatalf("blocked watch notice must not read as an operation failure: %q", msg.Content)
		}
	}
}

// The same guarantee for the check-script case: a defective check must never be
// matched against the declared business patterns.
func TestExternalWatchDoesNotMatchPatternsForDefectiveCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	// The command prints the watch's own failure pattern while failing for a
	// diagnosable reason of its own. The business verdict must not be taken.
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		TaskID:                task.ID,
		RunID:                 run.ID,
		Channel:               "cli",
		Description:           "Cloud Build reruns",
		CWD:                   t.TempDir(),
		Command:               `echo BUILD_FAILED; echo "gcloudx: command not found" >&2; exit 127`,
		SuccessPattern:        "ALL_SUCCESS",
		FailurePattern:        "BUILD_FAILED",
		IntervalSeconds:       5,
		CommandTimeoutSeconds: 10,
		TimeoutAt:             time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon.runExternalWatchPass(ctx)

	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("watch = %+v err=%v", stored, err)
	}
	reason, ok := watchCheckDefect(stored.LastError)
	if !ok || reason != watchReasonInvalidCheck {
		t.Fatalf("a defective check was treated as a business verdict: status=%q err=%q", stored.Status, stored.LastError)
	}
	queued, err := store.GetQueuedByIdempotencyKey(ctx, stored.TenantID, externalWatchFinalizationKey(*stored))
	if err != nil {
		t.Fatal(err)
	}
	if queued != nil && !strings.Contains(queued.Content, "real state is unknown") {
		t.Fatalf("finalization prompt let a blocked check imply a verdict: %q", queued.Content)
	}
}

// describeWatch renders the durable row behind a watch assertion. A watch that
// stays "running" has already recorded why: the checker/operation/verification
// verdicts, the attempt count, and when the next check was due.
func describeWatch(ctx context.Context, t *testing.T, store *control.Store, tenantID, watchID string) string {
	t.Helper()
	row, err := store.GetExternalWatch(ctx, tenantID, watchID)
	if err != nil || row == nil {
		return fmt.Sprintf("  <watch row unavailable: %v>", err)
	}
	return fmt.Sprintf(
		"  status:              %q\n"+
			"  last_output:         %q\n"+
			"  last_error:          %q\n"+
			"  checker_status:      %q\n"+
			"  operation_status:    %q\n"+
			"  verification_status: %q\n"+
			"  attempts:            %d\n"+
			"  command:             %q\n"+
			"  success_pattern:     %q\n"+
			"  cwd:                 %q\n"+
			"  next_check_in:       %s\n"+
			"  timeout_in:          %s",
		row.Status, row.LastOutput, row.LastError,
		row.CheckerStatus, row.OperationStatus, row.VerificationStatus,
		row.Attempts, row.Command, row.SuccessPattern, row.CWD,
		time.Until(row.NextCheckAt).Round(time.Millisecond),
		time.Until(row.TimeoutAt).Round(time.Millisecond))
}

// A Run can be cancelled more than once: a daemon shutdown, then the person.
// Only the most recent cancellation describes the Run now, so an older
// gateway shutdown must not excuse retrying work the person has since stopped.
func TestExternalWatchFinalizationHonorsNewestCancellationOfTheSameRun(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, run := seedConcludedWatch(t, store, identity, task)
	row, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: watch.TenantID, PersonID: watch.PersonID, Channel: watch.Channel,
		Platform: "cli", Content: "old finalization", TaskID: watch.TaskID,
		IdempotencyKey: externalWatchFinalizationKey(*watch),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, watch.TenantID, row.ID, control.QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	// Oldest first: the daemon restarted, and only afterwards did the person
	// stop this exact Run.
	for _, payload := range []string{`{"reason":"gateway shutdown"}`, `{"reason":"user requested stop"}`} {
		if _, err := store.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: run.ID, Type: "run.cancelled", Payload: json.RawMessage(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}

	daemon.reconcileExternalWatchFinalizations(ctx)
	row, err = store.GetQueued(ctx, watch.TenantID, row.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusDone {
		t.Fatalf("newest cancellation is the person's decision: queue = %+v, %v; want done", row, err)
	}
}
