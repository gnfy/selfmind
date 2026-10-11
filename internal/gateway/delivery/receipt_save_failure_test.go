package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestAcceptedNoticeReceiptFailureNeverBlindRetries(t *testing.T) {
	for _, blockUnknown := range []bool{false, true} {
		for _, catchUp := range []bool{false, true} {
			t.Run(fmt.Sprintf("block_unknown=%t/catch_up=%t", blockUnknown, catchUp), func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				store, err := control.OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if _, err := store.BindAccount(ctx, "default", "p1", "weixin", "wx-user", "fixture"); err != nil {
					t.Fatal(err)
				}
				raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				predicate := "NEW.status='sent'"
				if blockUnknown {
					predicate += " OR NEW.status='sent_unconfirmed'"
				}
				if _, err := raw.Exec(`CREATE TRIGGER fail_receipt BEFORE UPDATE ON outbound_messages WHEN ` + predicate + ` BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`); err != nil {
					t.Fatal(err)
				}
				choice := pendingNoticeChoice(t, store)
				sender := &switchableReceiptSender{confirmed: true}
				svc := NewService(store, sender, Options{PollInterval: time.Hour})
				msg := Message{TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-user", Channel: "wx-chat", Kind: KindTurnChoice, ChoiceID: choice.ID, Content: "choose exact work", LogicalKey: "turn-choice:" + choice.ID}
				id := choice.ID + "__notice_1"
				if catchUp {
					if accepted, err := svc.Enqueue(ctx, msg); !accepted || err != nil {
						t.Fatalf("enqueue: %t %v", accepted, err)
					}
					if _, err := raw.Exec(`UPDATE outbound_messages SET status='pending_session' WHERE id=?`, id); err != nil {
						t.Fatal(err)
					}
					svc.CatchUpRecoverable(ctx, "default", "p1", "weixin", "wx-user", "wx-chat")
				} else {
					if accepted, err := svc.EnqueueAndTryAccepted(ctx, msg); !accepted || err == nil {
						t.Fatalf("fault not applied: %t %v", accepted, err)
					}
				}
				if len(sender.sent) != 1 {
					t.Fatalf("send count before recovery: %d", len(sender.sent))
				}
				if _, err := raw.Exec(`DROP TRIGGER fail_receipt`); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`UPDATE outbound_messages SET updated_at=? WHERE id=?`, time.Now().Add(-time.Hour).Unix(), id); err != nil {
					t.Fatal(err)
				}
				svc.flushDue(ctx)
				status, err := store.DeliveryStatus(ctx, id)
				if err != nil || status != "sent_unconfirmed" || len(sender.sent) != 1 {
					t.Fatalf("accepted send blindly replayed or hidden: status=%s sends=%d %v", status, len(sender.sent), err)
				}
				if catchUp { // Its single inbound claim was already consumed.
					svc.CatchUpRecoverable(ctx, "default", "p1", "weixin", "wx-user", "wx-chat")
					if len(sender.sent) != 1 {
						t.Fatal("receipt save failure reopened the catch-up claim")
					}
				}
			})
		}
	}
}
