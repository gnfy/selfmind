package httpapi

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
)

func TestRecoveryNotificationDeliveredOnce(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "long-running deployment")
	if err != nil {
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
	if !strings.Contains(msg.Content, "saved task is safe and resumable") {
		t.Fatalf("content = %q", msg.Content)
	}

	daemon.sweepRecoveryNotifications()
	if len(recorder.messages) != 1 {
		t.Fatalf("recovery notification was delivered twice: %+v", recorder.messages)
	}
}

func TestExternalWatchCompletesOutsideAgentRun(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_watch", "WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
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

	daemon.runExternalWatchPass(ctx)
	counts, err := store.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID)
	if err != nil || counts[control.ExternalWatchSucceeded] != 1 {
		t.Fatalf("watch counts = %+v err=%v", counts, err)
	}
	updated, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || updated == nil {
		t.Fatalf("task = %+v err=%v", updated, err)
	}
	// The durable finalization row is drained immediately. Depending on
	// scheduler timing the task is either waiting_finalization or already
	// running that finalization; both prove it was not falsely blocked.
	if (updated.Status != "waiting_finalization" && updated.Status != "running") || !strings.Contains(updated.CurrentSummary, "CI build completed") {
		t.Fatalf("task after watch = %+v", updated)
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
		want := "Watcher " + watch.ID + " | status: succeeded | task: waiting_finalization"
		if msg.Content != want {
			t.Fatalf("unexpected external watch notice: %q", msg.Content)
		}
	}
	if !foundNotice {
		t.Fatalf("external watch completion notice missing: %+v", recorder.messages)
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
	row, err := store.GetQueuedByIdempotencyKey(ctx, externalWatchFinalizationKey(watch))
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

func TestExternalWatchFinalizationReconcilesDoneQueue(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Channel: "cli-session", Description: "CI build", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish watch = %v, %v", finished, err)
	}
	finished, err := store.ListExternalWatchesFinishedSince(ctx, control.ExternalWatchSucceeded, time.Now().Add(-time.Minute), 10)
	if err != nil || len(finished) != 1 {
		t.Fatalf("finished watches = %+v, %v", finished, err)
	}
	watch = &finished[0]
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
	if err := store.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "in_progress", "old status", nil); err != nil {
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
	updated, err := store.GetTask(ctx, watch.TenantID, watch.TaskID)
	if err != nil || updated == nil || updated.Status != "waiting_finalization" {
		t.Fatalf("reconciled task = %+v, %v", updated, err)
	}
}

func TestExternalWatchFinalizationRepairsLegacyGatewayShutdownCancellation(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Channel: "cli-session", Description: "CI build", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish watch = %v, %v", finished, err)
	}
	finished, err := store.ListExternalWatchesFinishedSince(ctx, control.ExternalWatchSucceeded, time.Now().Add(-time.Minute), 10)
	if err != nil || len(finished) != 1 {
		t.Fatalf("finished watches = %+v, %v", finished, err)
	}
	watch = &finished[0]
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
	for _, payload := range []map[string]interface{}{
		{"reason": "gateway shutdown"},
		{"error": "context canceled", "outcome": map[string]interface{}{"status": "cancelled"}},
	} {
		encoded, _ := json.Marshal(payload)
		if _, err := store.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: legacyRunID, Type: "run.cancelled", Payload: encoded,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "cancelled", "The run was cancelled before completion.", nil); err != nil {
		t.Fatal(err)
	}

	daemon.reconcileExternalWatchFinalizations(ctx)
	row, err = store.GetQueued(ctx, watch.TenantID, row.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusQueued {
		t.Fatalf("legacy cancelled queue = %+v, %v; want queued", row, err)
	}
	updated, err := store.GetTask(ctx, watch.TenantID, watch.TaskID)
	if err != nil || updated == nil || updated.Status != "waiting_finalization" {
		t.Fatalf("legacy cancelled task = %+v, %v; want waiting_finalization", updated, err)
	}
}

func TestExternalWatchFinalizationDoesNotReopenLaterUserCancellation(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Channel: "cli-session", Description: "CI build", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish watch = %v, %v", finished, err)
	}
	finished, err := store.ListExternalWatchesFinishedSince(ctx, control.ExternalWatchSucceeded, time.Now().Add(-time.Minute), 10)
	if err != nil || len(finished) != 1 {
		t.Fatalf("finished watches = %+v, %v", finished, err)
	}
	watch = &finished[0]
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
	for _, event := range []control.Event{
		{TaskID: task.ID, RunID: "run_old_shutdown", Type: "run.cancelled", Payload: json.RawMessage(`{"reason":"gateway shutdown"}`)},
		{TaskID: task.ID, RunID: "run_old_shutdown", Type: "run.cancelled", Payload: json.RawMessage(`{"error":"context canceled"}`)},
		{TaskID: task.ID, RunID: "run_later_user_cancel", Type: "run.cancelled", Payload: json.RawMessage(`{"reason":"cancelled by user"}`)},
	} {
		if _, err := store.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "cancelled", "The run was cancelled before completion.", nil); err != nil {
		t.Fatal(err)
	}

	daemon.reconcileExternalWatchFinalizations(ctx)
	row, err = store.GetQueued(ctx, watch.TenantID, row.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusDone {
		t.Fatalf("explicitly cancelled queue = %+v, %v; want done", row, err)
	}
	updated, err := store.GetTask(ctx, watch.TenantID, watch.TaskID)
	if err != nil || updated == nil || updated.Status != "cancelled" {
		t.Fatalf("explicitly cancelled task = %+v, %v; want cancelled", updated, err)
	}
}
