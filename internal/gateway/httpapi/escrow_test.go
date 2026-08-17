package httpapi

// Pending-approval/clarify escrow. A detached CLI pushes immediately; an
// attached CLI gets the inline prompt first and escalates to IM only after T1.

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
)

type unconfirmedApprovalSender struct {
	messages []delivery.Message
}

func (s *unconfirmedApprovalSender) Send(_ context.Context, msg delivery.Message) error {
	s.messages = append(s.messages, msg)
	return nil
}

func (s *unconfirmedApprovalSender) SendWithReceipt(_ context.Context, msg delivery.Message) (bool, error) {
	s.messages = append(s.messages, msg)
	return false, nil
}

func countKind(msgs []delivery.Message, kind string) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// TestEscrowRepushesApprovalAfterCLIDetaches walks the whole lifecycle: created
// while attached (suppressed, notified_at empty) → presence expires → next
// sweep pushes immediately, without waiting for T1 → second sweep is a no-op.
func TestEscrowRepushesApprovalAfterCLIDetaches(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	// Approval raised while the CLI is attached: the initial push is suppressed
	// and notified_at stays empty.
	daemon.touchPresence(ctx, identity)
	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "session-uuid", approval)
	if len(recorder.messages) != 0 {
		t.Fatalf("attached CLI must suppress the initial push, got %+v", recorder.messages)
	}

	// Detached is strong evidence that nobody is at the terminal, so escrow
	// pushes immediately even though the attached-client T1 is not reached.
	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) }
	daemon.sweepPendingNotifications(time.Hour)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("detached escrow must push exactly once, got %d approval messages (%+v)", got, recorder.messages)
	}
	if recorder.messages[0].Platform != "weixin" {
		t.Fatalf("escrow must target the preferred IM, got %s", recorder.messages[0].Platform)
	}

	// Second sweep is a no-op (notified_at is stamped).
	daemon.sweepPendingNotifications(time.Hour)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("escrow must be idempotent, got %d approval messages", got)
	}
}

// TestEscrowSkipsWhileCLIAttached: an un-notified pending approval is left alone
// while the person is still at the CLI.
func TestEscrowSkipsWhileCLIAttached(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.touchPresence(ctx, identity) // attached
	daemon.sweepPendingNotifications(time.Hour)
	if len(recorder.messages) != 0 {
		t.Fatalf("attached CLI below T1 must suppress escrow, got %+v", recorder.messages)
	}

	// If the person leaves the prompt unanswered past T1, escalate even though
	// the process is still attached. This is rare but prevents an open terminal
	// from suppressing mobile notification forever.
	time.Sleep(3 * time.Millisecond)
	daemon.sweepPendingNotifications(time.Millisecond)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("attached CLI past T1 must escrow once, got %d (%+v)", got, recorder.messages)
	}
}

func TestPhoneFirstEscrowBypassesT1AfterNoDurableInitialAttempt(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalSurface, "phone-first"); err != nil {
		t.Fatal(err)
	}
	daemon.touchPresence(ctx, identity)

	// Model the narrow window where the phone-first request exists before a
	// delivery service/route can create its durable outbox row.
	daemon.Delivery = nil
	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "cli-session", approval)

	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.sweepPendingNotifications(time.Hour)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("phone-first escrow below T1 must make the missing initial attempt, got %d (%+v)", got, recorder.messages)
	}

	// Confirmed delivery stamps notified_at, so later sweeps stay idempotent.
	daemon.sweepPendingNotifications(time.Hour)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("confirmed phone-first escrow must not repeat, got %d (%+v)", got, recorder.messages)
	}
}

func TestPhoneFirstEscrowDoesNotBlindlyReplaySentUnconfirmed(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalSurface, "phone-first"); err != nil {
		t.Fatal(err)
	}
	recorder := &unconfirmedApprovalSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.touchPresence(ctx, identity)

	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "cli-session", approval)
	if len(recorder.messages) != 1 {
		t.Fatalf("phone-first initial attempt = %d, want 1", len(recorder.messages))
	}
	state, err := store.LatestDeliveryEndpointState(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123")
	if err != nil || state == nil || state.Status != "sent_unconfirmed" {
		t.Fatalf("delivery state=%+v err=%v, want sent_unconfirmed", state, err)
	}

	// phone-first bypasses T1, but the same durable idempotency row is terminal
	// for blind retry. Only a fresh inbound/session-aware catch-up may replay it.
	daemon.sweepPendingNotifications(time.Hour)
	daemon.sweepPendingNotifications(time.Hour)
	if len(recorder.messages) != 1 {
		t.Fatalf("escrow blindly replayed sent_unconfirmed: messages=%d (%+v)", len(recorder.messages), recorder.messages)
	}
	pending, err := store.ListPendingApprovalsForEscrow(ctx, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range pending {
		found = found || pending[i].ID == approval.ID
	}
	if !found {
		t.Fatal("sent_unconfirmed must not be mislabeled as confirmed notification")
	}
}

// TestEscrowRepushesClarify: the clarify path escrows exactly like approvals.
func TestEscrowRepushesClarify(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	// Keep the helper approval out of the way so this test isolates clarifies.
	if err := store.MarkApprovalNotified(ctx, identity.TenantID, approval.ID); err != nil {
		t.Fatal(err)
	}
	clarify, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, Question: "which port?",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) } // detached
	daemon.sweepPendingNotifications(time.Hour)
	if got := countKind(recorder.messages, delivery.KindClarify); got != 1 {
		t.Fatalf("escrow must push the clarify once, got %d (%+v)", got, recorder.messages)
	}

	// notified_at is set → the question drops out of the escrow scan.
	pending, err := store.ListPendingClarifiesForEscrow(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pending {
		if c.ID == clarify.ID {
			t.Fatal("escrowed clarify must be marked notified")
		}
	}
}

// TestEscrowDisabledByZeroThreshold: threshold 0 disables the pass entirely.
func TestEscrowDisabledByZeroThreshold(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) } // detached

	daemon.sweepPendingNotifications(0)
	if len(recorder.messages) != 0 {
		t.Fatalf("threshold 0 must disable escrow, got %+v", recorder.messages)
	}
}
