package delivery

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
)

func pendingNoticeChoice(t *testing.T, store *control.Store) *control.PendingTurnChoice {
	t.Helper()
	choice, err := store.CreatePendingTurnChoice(context.Background(), control.PendingTurnChoiceCreate{
		TenantID: "default", PersonID: "p1", Channel: "wx-chat", RequestJSON: `{"content":"inspect the other service"}`,
		Options: []control.TurnChoiceOption{{Key: "1", Label: "New work", Action: "new"}, {Key: "2", Label: "Other new work", Action: "new"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return choice
}

func TestTurnChoiceNoticeSurvivesRestartWithoutDuplicateParts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, "default", "p1", "weixin", "wx-user", "fixture"); err != nil {
		t.Fatal(err)
	}
	choice := pendingNoticeChoice(t, store)
	msg := Message{TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-user", Channel: "wx-chat",
		Content: strings.Repeat("choose a work\n", 12), Kind: KindTurnChoice, ChoiceID: choice.ID, LogicalKey: "turn-choice:" + choice.ID}
	svc := NewService(store, nil, Options{MaxMessageChars: 60})
	if accepted, err := svc.Enqueue(ctx, msg); !accepted || err != nil {
		t.Fatalf("enqueue: %t %v", accepted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var sends atomic.Int32
	svc = NewService(store, SenderFunc(func(_ context.Context, sent Message) error {
		if sent.ChoiceID != choice.ID {
			t.Errorf("lost typed choice reference: %+v", sent)
		}
		sends.Add(1)
		return nil
	}), Options{MaxMessageChars: 60})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.flushDue(ctx) }()
	}
	wg.Wait()
	want := int32(len(splitMessage(msg.Content, 60)))
	if sends.Load() != want {
		t.Fatalf("restart sent %d parts, want %d", sends.Load(), want)
	}
	if accepted, err := svc.EnqueueAndTryAccepted(ctx, msg); !accepted || err != nil {
		t.Fatalf("re-enqueue: %t %v", accepted, err)
	}
	if sends.Load() != want {
		t.Fatalf("logical replay duplicated parts: %d", sends.Load())
	}
}

func TestAnsweredTurnChoiceNoticeIsNotSent(t *testing.T) {
	ctx := context.Background()
	store, err := openDeliveryFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	choice := pendingNoticeChoice(t, store)
	var sends atomic.Int32
	svc := NewService(store, SenderFunc(func(context.Context, Message) error { sends.Add(1); return nil }), Options{})
	if accepted, err := svc.Enqueue(ctx, Message{TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-user",
		Channel: "wx-chat", Content: "choose a work", Kind: KindTurnChoice, ChoiceID: choice.ID}); !accepted || err != nil {
		t.Fatalf("enqueue: %t %v", accepted, err)
	}
	if _, _, err := store.ClaimPendingTurnChoice(ctx, "default", "p1", choice.ID, "1", time.Now()); err != nil {
		t.Fatal(err)
	}
	svc.flushDue(ctx)
	status, err := store.DeliveryStatus(ctx, choice.ID+"__notice_1")
	if err != nil || status != "superseded" || sends.Load() != 0 {
		t.Fatalf("obsolete choice sent: status=%s sends=%d %v", status, sends.Load(), err)
	}
}

func TestCrossEndpointChoiceNoticeTracksPendingChoice(t *testing.T) {
	for _, state := range []string{"pending", "answered", "expired"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			store, err := openDeliveryFixture(t)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			choice, err := store.CreatePendingTurnChoice(ctx, control.PendingTurnChoiceCreate{
				TenantID: "default", PersonID: "p1", Channel: "cli:origin", RequestJSON: `{"content":"continue another task"}`,
				Options: []control.TurnChoiceOption{{Key: "1", Label: "First task", Action: "new"}, {Key: "2", Label: "Second task", Action: "new"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var sends atomic.Int32
			svc := NewService(store, SenderFunc(func(_ context.Context, msg Message) error {
				if msg.Kind != KindRecovery || msg.ChoiceID != choice.ID {
					t.Errorf("lost cross-endpoint notice boundary: %+v", msg)
				}
				sends.Add(1)
				return nil
			}), Options{})
			rows := svc.PrepareChoiceNotice(Message{TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-user", Channel: "wx-chat",
				Content: "Use /choose " + choice.ID + " 1 to select this saved request.", Kind: KindRecovery, ChoiceID: choice.ID})
			if _, err := store.EnqueueDeliveries(ctx, rows); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "answered":
				if _, _, err := store.ClaimPendingTurnChoice(ctx, "default", "p1", choice.ID, "1", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, _, err := store.PruneTurnContinuity(ctx, choice.ExpiresAt.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			svc.flushDue(ctx)
			wantStatus, wantSends := "superseded", int32(0)
			if state == "pending" {
				wantStatus, wantSends = "sent", 1
			}
			status, err := store.DeliveryStatus(ctx, rows[0].ID)
			if err != nil || status != wantStatus || sends.Load() != wantSends {
				t.Fatalf("%s choice notice: status=%s sends=%d %v", state, status, sends.Load(), err)
			}
		})
	}
}

func TestParallelIMResultsCarryExactRunReference(t *testing.T) {
	ctx := context.Background()
	store, err := openDeliveryFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var contents []string
	svc := NewService(store, SenderFunc(func(_ context.Context, msg Message) error { contents = append(contents, msg.Content); return nil }), Options{})
	for _, runID := range []string{"run_alpha", "run_beta"} {
		if err := svc.EnqueueAndTry(ctx, Message{TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-user", Channel: "wx-chat", RunID: runID, Content: "Finished", Kind: KindFinalResult}); err != nil {
			t.Fatal(err)
		}
	}
	if len(contents) != 2 || contents[0] != "Run run_alpha\nFinished" || contents[1] != "Run run_beta\nFinished" {
		t.Fatalf("IM work identities are ambiguous: %q", contents)
	}
}
