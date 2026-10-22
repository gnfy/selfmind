package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
)

func TestTurnChoiceNoticeIsDurableWhenSenderIsUnavailable(t *testing.T) {
	for _, platform := range []string{"weixin", "telegram"} {
		t.Run(platform, func(t *testing.T) {
			user := map[string]string{"weixin": "wx-local", "telegram": "tg-local"}[platform]
			f := newParkedChoiceFixture(t, platform, user, "chat")
			f.daemon.Delivery = delivery.NewService(f.store, delivery.SenderFunc(func(context.Context, delivery.Message) error {
				return errors.New("offline test sender")
			}), delivery.Options{PollInterval: time.Hour})
			resp, code := f.daemon.ProcessMessage(context.Background(), api.MessageRequest{
				Platform: platform, PlatformUserID: user, Channel: "chat", Content: "also inspect the other service", Async: true,
			})
			if code != 200 || resp.Choice == nil || !resp.DeliveryEnqueued {
				t.Fatalf("request did not produce the unresolved choice: %d %+v", code, resp)
			}
			rows, err := f.store.ListDueDeliveries(context.Background(), 10)
			found := false
			for _, row := range rows {
				if strings.HasPrefix(row.ID, resp.Choice.ID+"__notice_") && row.Kind == "turn_choice" && row.Content == resp.Content {
					found = true
				}
			}
			if err != nil || !found {
				t.Fatalf("a saved choice lost its outbound prompt: %+v %v", rows, err)
			}
		})
	}
}

func TestTurnChoiceNoticeSaveFailureHoldsInboundAndKeepsExactChoice(t *testing.T) {
	ctx := context.Background()
	f := newParkedChoiceFixture(t, "weixin", "wx-local", "chat")
	// Only the outbox connection is unavailable. Input and choice persistence
	// still work, so the response must not claim the input can be redispatched.
	closed := controltest.NewStore(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	f.daemon.Delivery = delivery.NewService(closed, delivery.SenderFunc(func(context.Context, delivery.Message) error { t.Fatal("sent before saving"); return nil }), delivery.Options{})
	inbound := api.DurableInbound{MessageID: "choice-notice-fault", RawPayload: []byte(`{"id":"choice-notice-fault"}`), Request: api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-local", Channel: "chat", Content: "also inspect the third service", Async: true,
	}}
	resp, code, err := f.daemon.ProcessDurableInbound(ctx, inbound)
	if code != 503 || !errors.Is(err, api.ErrInboundHeld) || resp.Choice == nil || resp.DeliveryEnqueued || !strings.Contains(resp.Error, "/choose "+resp.Choice.ID) {
		t.Fatalf("unsaved notice acknowledged or lost recovery controls: code=%d err=%v resp=%+v", code, err, resp)
	}
	if _, err := f.store.PeekPendingTurnChoice(ctx, f.owner.TenantID, f.owner.PersonID, resp.Choice.ID, time.Now()); err != nil {
		t.Fatalf("held input was lost: %v", err)
	}
	state, _, err := f.store.InboundReceipt(ctx, "weixin", inbound.MessageID)
	if err != nil || state != control.InboundDispatching {
		t.Fatalf("receipt became redispatchable/accepted: %s %v", state, err)
	}
	f.requireNothingSteered(t, "outbox save failure")
}
