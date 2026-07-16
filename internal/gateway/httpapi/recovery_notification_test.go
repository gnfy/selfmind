package httpapi

import (
	"context"
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
	run, err := store.StartRun(ctx, task, "cli", "monitor the external build")
	if err != nil {
		t.Fatal(err)
	}
	command := "printf READY"
	if runtime.GOOS == "windows" {
		command = "Write-Output READY"
	}
	_, err = store.CreateExternalWatch(ctx, control.ExternalWatch{
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
	if updated.Status != "in_progress" || !strings.Contains(updated.CurrentSummary, "CI build completed") {
		t.Fatalf("task after watch = %+v", updated)
	}
	if !hasEventOfType(t, store, task.ID, "external_watch.completed") {
		t.Fatal("external watch completion event missing")
	}
}
