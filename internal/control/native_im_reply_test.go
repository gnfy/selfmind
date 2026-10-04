package control

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestNativeIMReplyEdgeIsScopedAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: "person-a", Platform: "telegram", PlatformUserID: "user-a",
		Channel: "chat-1", RunID: "run-a", ApprovalID: "apr-a", ClarifyID: "clarify-a", Content: "question",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDelivery(ctx, message.ID)
	if err != nil || !claimed {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	if err := store.MarkDeliverySentWithNativeID(ctx, message.ID, "123"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	edge, err := store.NativeIMReplyTarget(ctx, "default", "person-a", "telegram", "chat-1", "123")
	if err != nil || edge.RunID != "run-a" || edge.ApprovalID != "apr-a" || edge.ClarifyID != "clarify-a" {
		t.Fatalf("reply edge after restart: %+v, %v", edge, err)
	}
	for _, query := range []struct{ tenant, person, platform, channel, id string }{
		{"other", "person-a", "telegram", "chat-1", "123"},
		{"default", "person-b", "telegram", "chat-1", "123"},
		{"default", "person-a", "telegram", "chat-2", "123"},
		{"default", "person-a", "weixin", "chat-1", "123"},
		{"default", "person-a", "telegram", "chat-1", "124"},
	} {
		if _, err := store.NativeIMReplyTarget(ctx, query.tenant, query.person, query.platform, query.channel, query.id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("foreign reply %+v resolved: %v", query, err)
		}
	}
}

func TestNativeIMReplyIDCollisionDoesNotChangeDelivery(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, person := range []string{"person-a", "person-b"} {
		d, err := store.EnqueueDelivery(ctx, Delivery{PersonID: person, Platform: "telegram", Channel: "shared", RunID: person, Content: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if claimed, err := store.ClaimDelivery(ctx, d.ID); err != nil || !claimed {
			t.Fatalf("claim %s: %v, %v", person, claimed, err)
		}
		err = store.MarkDeliverySentWithNativeID(ctx, d.ID, "42")
		if person == "person-a" && err != nil {
			t.Fatal(err)
		}
		if person == "person-b" {
			if err == nil {
				t.Fatal("native id collision must not overwrite the first person's edge")
			}
			status, err := store.DeliveryStatus(ctx, d.ID)
			if err != nil || status != "sending" {
				t.Fatalf("colliding delivery state: %q, %v", status, err)
			}
		}
	}
}
