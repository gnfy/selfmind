package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestGatewayOwnsNativeInboundIdentityAndReceipts(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: "default"}
	request := api.MessageRequest{TenantID: "default", Platform: "weixin", PlatformUserID: "wx-owner",
		Channel: "wx-owner", Content: "/status"}
	inbound := api.DurableInbound{Request: request, MessageID: "wx-1", RawPayload: []byte(`{"message_id":"wx-1"}`), OwnerPersonID: owner.PersonID}
	if _, status, err := server.ProcessDurableInbound(ctx, inbound); err != nil || status != http.StatusOK {
		t.Fatalf("first dispatch: status=%d err=%v", status, err)
	}
	bound, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil || bound.PersonID != owner.PersonID {
		t.Fatalf("gateway owner binding: %+v %v", bound, err)
	}
	if reply, status, err := server.ProcessDurableInbound(ctx, inbound); err != nil || status != http.StatusOK || reply.Content != "" {
		t.Fatalf("accepted duplicate was dispatched: %+v %d %v", reply, status, err)
	}

	unknown := api.DurableInbound{Request: api.MessageRequest{TenantID: "default", Platform: "weixin",
		PlatformUserID: "wx-other", Channel: "wx-other", Content: "/status"},
		MessageID: "wx-2", RawPayload: []byte(`{"message_id":"wx-2"}`)}
	if _, status, err := server.ProcessDurableInbound(ctx, unknown); err != nil || status != http.StatusOK {
		t.Fatalf("separate sender: status=%d err=%v", status, err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-other", "")
	if err != nil || other.PersonID == owner.PersonID {
		t.Fatalf("unknown sender inherited owner: %+v %v", other, err)
	}

	uncertain := api.DurableInbound{Request: request, MessageID: "wx-uncertain", RawPayload: []byte(`{"message_id":"wx-uncertain"}`)}
	if _, err := store.BeginInbound(ctx, "weixin", uncertain.MessageID, uncertain.RawPayload,
		control.InboundOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimInbound(ctx, "weixin", uncertain.MessageID); err != nil || !claimed {
		t.Fatalf("pre-dispatch claim: %t %v", claimed, err)
	}
	if _, _, err := server.ProcessDurableInbound(ctx, uncertain); !errors.Is(err, api.ErrInboundUncertain) {
		t.Fatalf("uncertain receipt was replayed: %v", err)
	}
}
