package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
)

// hangingSender is a messaging platform whose API never answers until released.
type hangingSender struct {
	release chan struct{}
	mu      sync.Mutex
	sent    []delivery.Message
}

func newHangingSender(t *testing.T) *hangingSender {
	t.Helper()
	sender := &hangingSender{release: make(chan struct{})}
	t.Cleanup(sender.open)
	return sender
}

func (h *hangingSender) open() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.release:
	default:
		close(h.release)
	}
}

func (h *hangingSender) Send(ctx context.Context, msg delivery.Message) error {
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, msg)
	return nil
}

func (h *hangingSender) delivered() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, msg := range h.sent {
		if strings.Contains(msg.Content, "/queue drop") {
			count++
		}
	}
	return count
}

// The queue drain runs for every person in turn, inline from /approve and from
// the watcher goroutine. The notice that a held row will not start is therefore
// only recorded for delivery there: the outbox sends it and retries it, and a
// messaging platform that never answers cannot stall anyone's work.
func TestUnreadableAdmissionNoticeNeverBlocksTheDrainOnTheMessagingPlatform(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	hang := newHangingSender(t)
	// The probe server runs its own outbox loop on this store, with a sender
	// that answers at once. It must stop before the hanging platform replaces
	// it, or it sends the notice first and the outbox row this test looks for
	// is gone before it can look.
	s.daemon.Delivery.Stop()
	s.daemon.Delivery = delivery.NewService(s.store, hang, delivery.Options{PollInterval: 10 * time.Millisecond})
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := s.store.StartRunWithOptions(ctx, task, "cli", "publish the release", control.StartRunOptions{Admission: webAdmission("smart", "publish the release")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, parked.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	s.corruptRunAdmission(t, parked.ID)

	wait := control.ExternalResourceWait{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, RunID: parked.ID, EffectID: "effect-hang"}
	returned := make(chan error, 1)
	go func() { returned <- s.daemon.enqueueExternalResourceWake(ctx, wait, "waiting_external") }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain waited on a messaging platform that never answered")
	}
	if !hasEventOfType(t, s.store, task.ID, "queue.admission_unreadable") {
		t.Fatal("the held continuation left no durable notice on its work")
	}
	due, err := s.store.ListDueDeliveries(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, d := range due {
		if strings.Contains(d.Content, "/queue drop") {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("the push must wait in the outbox, once; found %d: %+v", queued, due)
	}

	// The outbox delivers it when the platform answers, and it is not pushed twice.
	s.daemon.Delivery.Start(ctx)
	t.Cleanup(s.daemon.Delivery.Stop)
	hang.open()
	waitUntil(t, 5*time.Second, func() bool { return hang.delivered() == 1 }, "the outbox never delivered the notice")
	s.daemon.coordinator().drainQueue(s.identity)
	time.Sleep(100 * time.Millisecond)
	if got := hang.delivered(); got != 1 {
		t.Fatalf("the person was pushed %d times, want once", got)
	}
}

// A push the outbox could not record is attempted again, though the thread
// record exists: the notice is settled only when the person has been told
// through the push as well as the record. Once recorded in the outbox it is
// never recorded twice, and the outbox owns its delivery.
func TestUnreadableAdmissionNoticeRetriesAPushTheOutboxRefused(t *testing.T) {
	s := newAdmissionProbeServer(t)
	s.daemon.coordinator().unreadableNoticeRetry = time.Nanosecond
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := s.store.StartRunWithOptions(ctx, task, "cli", "publish the release", control.StartRunOptions{Admission: webAdmission("smart", "publish the release")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, parked.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	s.corruptRunAdmission(t, parked.ID)
	s.execRaw(t, `CREATE TRIGGER fail_notice_outbox BEFORE INSERT ON outbound_messages BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END`)

	wait := control.ExternalResourceWait{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, RunID: parked.ID, EffectID: "effect-outbox"}
	if err := s.daemon.enqueueExternalResourceWake(ctx, wait, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	pushes := func() int {
		count := 0
		for _, msg := range s.sender.snapshot() {
			if strings.Contains(msg.Content, "/queue drop") {
				count++
			}
		}
		return count
	}
	if !hasEventOfType(t, s.store, task.ID, "queue.admission_unreadable") {
		t.Fatal("the thread record was not written")
	}
	time.Sleep(100 * time.Millisecond)
	if got := pushes(); got != 0 {
		t.Fatalf("the injected outbox failure did not apply: %d pushes", got)
	}

	s.execRaw(t, `DROP TRIGGER fail_notice_outbox`)
	s.daemon.coordinator().drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool { return pushes() == 1 }, "the refused push was not attempted again once the outbox recovered")
	s.daemon.coordinator().drainQueue(s.identity)
	time.Sleep(100 * time.Millisecond)
	if got := pushes(); got != 1 {
		t.Fatalf("the person was pushed %d times, want once", got)
	}
}
