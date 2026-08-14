package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// A person with no live endpoint and no bound account cannot answer, so the
// wait must collapse to the short budget instead of burning the run's time on
// a decision that has nowhere to come from.
func TestApprovalWaitBudgetUnattendedUsesShortBound(t *testing.T) {
	srv, _, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	unreachable := &control.IdentityContext{TenantID: identity.TenantID, PersonID: "person-with-no-endpoints"}

	if got := coordinator.approvalWaitBudget(context.Background(), unreachable); got != defaultApprovalWaitUnattended {
		t.Fatalf("unattended budget = %s, want %s", got, defaultApprovalWaitUnattended)
	}
}

// Presence expires after 90s, so a person who typed on IM and then looked away
// reads as detached while still being able to answer. Recent IM activity keeps
// the full budget; the fixture's CLI account alone must not.
func TestApprovalWaitBudgetRecentIMAccountCountsAsAttended(t *testing.T) {
	srv, store, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	bound, err := store.BindAccount(context.Background(), identity.TenantID, identity.PersonID, "weixin", "wx-user", "WX User")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAccountLastSeen(context.Background(), identity.TenantID, bound.AccountID); err != nil {
		t.Fatal(err)
	}

	if srv.presenceTracker().AnyAttached(identity.PersonID) {
		t.Fatal("fixture should start with no live attachment")
	}
	if got := coordinator.approvalWaitBudget(context.Background(), identity); got != defaultApprovalWait {
		t.Fatalf("recent IM account budget = %s, want %s", got, defaultApprovalWait)
	}
}

func TestApprovalWaitBudgetStaleBindingUsesShortBound(t *testing.T) {
	srv, store, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	if _, err := store.BindAccount(context.Background(), identity.TenantID, identity.PersonID, "weixin", "stale-wx-user", "Stale WX User"); err != nil {
		t.Fatal(err)
	}

	if got := coordinator.approvalWaitBudget(context.Background(), identity); got != defaultApprovalWaitUnattended {
		t.Fatalf("stale IM account budget = %s, want %s", got, defaultApprovalWaitUnattended)
	}
}

func TestApprovalWaitBudgetRecentIMWithNewerDeliveryFailureUsesShortBound(t *testing.T) {
	srv, store, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	bound, err := store.BindAccount(context.Background(), identity.TenantID, identity.PersonID, "weixin", "wx-unreachable", "WX User")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAccountLastSeen(context.Background(), identity.TenantID, bound.AccountID); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.EnqueueDelivery(context.Background(), control.Delivery{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       "weixin",
		PlatformUserID: "wx-unreachable",
		Channel:        "wx-unreachable",
		Content:        "approval needed",
		Kind:           "approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryPendingSession(context.Background(), delivery.ID, "iLink prepare failed: ret=-2"); err != nil {
		t.Fatal(err)
	}

	if got := coordinator.approvalWaitBudget(context.Background(), identity); got != defaultApprovalWaitUnattended {
		t.Fatalf("unreachable recent IM budget = %s, want %s", got, defaultApprovalWaitUnattended)
	}
}

func TestApprovalWaitBudgetPresenceCountsAsAttended(t *testing.T) {
	srv, _, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	unreachable := &control.IdentityContext{TenantID: identity.TenantID, PersonID: "person-with-no-endpoints"}
	srv.presenceTracker().Touch(unreachable.PersonID, "cli")

	if got := coordinator.approvalWaitBudget(context.Background(), unreachable); got != defaultApprovalWait {
		t.Fatalf("attached budget = %s, want %s", got, defaultApprovalWait)
	}
}

// The wait must end before the caller's deadline. Without the reserve the
// caller's context kills the waiter mid-cleanup and the run reports a bare
// transport timeout instead of parked work.
func TestApprovalWaitBudgetBoundedByCallerDeadline(t *testing.T) {
	srv, _, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	const callerBudget = 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()

	got := coordinator.approvalWaitBudget(ctx, identity)
	if got <= 0 || got > callerBudget-approvalWaitReserve {
		t.Fatalf("budget = %s, want a positive value at most %s", got, callerBudget-approvalWaitReserve)
	}
}

// A caller with less time than the reserve cannot afford the ask AND the turn
// that reports the parked work, so there is no budget to spend.
func TestApprovalWaitBudgetNoTimeLeft(t *testing.T) {
	srv, _, identity, _, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	ctx, cancel := context.WithTimeout(context.Background(), approvalWaitReserve/2)
	defer cancel()

	if got := coordinator.approvalWaitBudget(ctx, identity); got > 0 {
		t.Fatalf("budget = %s, want non-positive when the caller has less time than the reserve", got)
	}
}

// With no budget the handler must park immediately and WITHOUT creating a
// durable row: a request that would expire in the same breath is noise on
// every later /approvals list, and its push notification is noise too.
func TestToolApprovalHandlerParksWhenNoBudget(t *testing.T) {
	srv, store, identity, task, seeded := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	ctx, cancel := context.WithTimeout(context.Background(), approvalWaitReserve/2)
	defer cancel()

	decision, err := coordinator.toolApprovalHandler(identity, task, nil, "cli")(ctx, tools.ToolApprovalRequest{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   task.ID,
		ToolName: "terminal",
		Reason:   "arbitrary code execution requires approval",
		Args:     map[string]interface{}{"command": "go test ./..."},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if decision.Approved {
		t.Fatal("running out of time must never approve")
	}
	if decision.Outcome != tools.ApprovalOutcomeTimedOut {
		t.Fatalf("outcome = %q, want %q", decision.Outcome, tools.ApprovalOutcomeTimedOut)
	}

	pending, err := store.ListApprovalRequests(context.Background(), identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	for _, row := range pending {
		if row.ID != seeded.ID {
			t.Fatalf("a no-budget ask must not create a durable row, found %s", row.ID)
		}
	}

	events, err := store.ListTaskEvents(context.Background(), task.ID, 20)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Type == "approval.skipped_no_budget" {
			found = true
		}
	}
	if !found {
		t.Fatal("a skipped ask must stay visible as a run event")
	}
}

// The regression this whole batch exists for: a caller deadline shorter than
// the approval budget must still yield a typed decision, with the caller's
// context still alive to receive it.
func TestToolApprovalHandlerReturnsBeforeCallerDeadline(t *testing.T) {
	srv, _, identity, task, _ := newApprovalTestServer(t)
	coordinator := &RunCoordinator{srv: srv}
	// Long enough to leave a real wait after the reserve, short enough to keep
	// the test quick: the waiter must return on its own budget, not on the
	// caller's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), approvalWaitReserve+5*time.Second)
	defer cancel()

	start := time.Now()
	decision, err := coordinator.toolApprovalHandler(identity, task, nil, "cli")(ctx, tools.ToolApprovalRequest{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   task.ID,
		ToolName: "terminal",
		Reason:   "arbitrary code execution requires approval",
		Args:     map[string]interface{}{"command": "go test ./..."},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("waiter outlived the caller deadline (elapsed %s)", elapsed)
	}
	if decision.Approved || decision.Outcome != tools.ApprovalOutcomeTimedOut {
		t.Fatalf("decision = %+v, want an unapproved timeout", decision)
	}
	if decision.Reason == "" {
		t.Fatal("a timeout must explain that the work is parked, not rejected")
	}
}
