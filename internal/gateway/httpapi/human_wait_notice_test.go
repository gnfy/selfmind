package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/delivery"
)

// TestHumanWaitEventReportsWhetherTheLiveSurfaceSawIt is the mechanism behind
// the invisible approval. The push fallback is suppressed while a CLI is
// attached, justified by "the live TUI already shows it" — so the notification
// path must be told whether the live event ACTUALLY landed, not merely that a
// live surface exists. Silencing both left a production release parked for 21
// minutes with nothing on any channel.
func TestHumanWaitEventReportsWhetherTheLiveSurfaceSawIt(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: "default", PersonID: "p1", Title: "release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "deploy")
	if err != nil {
		t.Fatal(err)
	}

	if !emitHumanWaitEvent(ctx, store, control.Event{
		RunID: run.ID, Type: "approval.requested", Visibility: "task", Channel: "cli",
		Payload: mustJSON(map[string]interface{}{"approval_id": "apr_x"}),
	}, "approval_id", "apr_x") {
		t.Fatal("a valid human-wait event should report the live surface informed")
	}

	// An event that cannot be owned by any run or thread must report NOT
	// informed, so the caller un-suppresses the push instead of trusting a
	// surface that never received it.
	if emitHumanWaitEvent(ctx, store, control.Event{
		RunID: "run_does_not_exist", Type: "approval.requested", Visibility: "task",
	}, "approval_id", "apr_y") {
		t.Fatal("a dropped human-wait event must not claim the live surface was informed")
	}

	// And a nil store is the same answer, not a panic.
	if emitHumanWaitEvent(ctx, nil, control.Event{RunID: run.ID, Type: "approval.requested"}, "approval_id", "apr_z") {
		t.Fatal("no store means the live surface was not informed")
	}
}

// TestSuppressionRequiresTheLiveSurfaceToHaveSeenIt closes the window that made
// a parked approval invisible. The attached-CLI push is suppressed because the
// TUI shows the inline prompt — but when the event that BUILDS that prompt
// could not be written, the justification is gone and the person must be told
// some other way. Escrow eventually escalates, only after they detach or the
// request ages out; that is no help to someone watching a spinner.
func TestSuppressionRequiresTheLiveSurfaceToHaveSeenIt(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.touchPresence(ctx, identity)

	// Live surface informed: the established suppression stands.
	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "cli", approval, true)
	if len(recorder.messages) != 0 {
		t.Fatalf("an informed live surface must still suppress the push; got %+v", recorder.messages)
	}

	// Live surface NOT informed, same attached CLI: the push must go out.
	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "cli", approval, false)
	if len(recorder.messages) != 1 {
		t.Fatalf("a dropped live event must un-suppress the push; got %+v", recorder.messages)
	}
	if recorder.messages[0].Platform != "weixin" {
		t.Fatalf("fallback should reach the preferred IM endpoint; got %+v", recorder.messages[0])
	}
}
