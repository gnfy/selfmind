package delivery

import (
	"context"
	"selfmind/internal/control"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueuedDeliveryCannotFollowReboundEndpoint(t *testing.T) {
	for _, rebound := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-person", true: "other-person"}[rebound], func(t *testing.T) {
			ctx := context.Background()
			store, err := control.OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			original, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "peer", "original")
			if err != nil {
				t.Fatal(err)
			}
			var sends atomic.Int32
			svc := NewService(store, SenderFunc(func(context.Context, Message) error { sends.Add(1); return nil }), Options{})
			if _, err := svc.Enqueue(ctx, Message{TenantID: "default", PersonID: original.PersonID, Platform: "weixin", PlatformUserID: "peer", Channel: "direct", Content: "private result", Kind: KindFinalResult}); err != nil {
				t.Fatal(err)
			}
			person := original.PersonID
			if rebound {
				other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "other")
				if err != nil {
					t.Fatal(err)
				}
				person = other.PersonID
			}
			if _, err := store.BindAccount(ctx, "default", person, "weixin", "peer", "rebound"); err != nil {
				t.Fatal(err)
			}
			svc.flushDue(ctx)
			want := int32(1)
			if rebound {
				want = 0
			}
			if sends.Load() != want {
				t.Fatalf("sends=%d want=%d", sends.Load(), want)
			}
			if rebound {
				rows, err := store.ListUndeliveredOutbound(ctx, "default", original.PersonID, time.Time{}, 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].Status != "failed" || rows[0].LastError == "" {
					t.Fatalf("missing ownership diagnostic: %+v", rows)
				}
			}
		})
	}
}

func TestCatchUpCannotResendToReboundEndpoint(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "peer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	sender := &alwaysUnconfirmed{calls: &sends}
	svc := NewService(store, sender, Options{})
	if err := svc.EnqueueAndTry(ctx, Message{TenantID: "default", PersonID: owner.PersonID, Platform: "weixin", PlatformUserID: "peer", Channel: "direct", Content: "private result", Kind: KindFinalResult}); err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, "default", other.PersonID, "weixin", "peer", "other"); err != nil {
		t.Fatal(err)
	}
	if got := svc.CatchUpRecoverable(ctx, "default", owner.PersonID, "weixin", "peer", "direct"); got != 0 {
		t.Fatalf("rebound result confirmed=%d", got)
	}
	if sends.Load() != 1 {
		t.Fatalf("rebound catch-up leaked, sends=%d", sends.Load())
	}
}

type alwaysUnconfirmed struct{ calls *atomic.Int32 }

func (s *alwaysUnconfirmed) Send(ctx context.Context, msg Message) error {
	_, err := s.SendWithReceipt(ctx, msg)
	return err
}
func (s *alwaysUnconfirmed) SendWithReceipt(context.Context, Message) (bool, error) {
	s.calls.Add(1)
	return false, nil
}
