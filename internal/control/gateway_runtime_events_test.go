package control

import (
	"context"
	"testing"
)

func TestGatewayRuntimeEventIsIdempotentPerInstance(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	event := GatewayRuntimeEvent{InstanceID: "gateway_previous", EventType: "gateway.unclean_exit"}
	if inserted, err := store.RecordGatewayRuntimeEvent(ctx, event); err != nil || !inserted {
		t.Fatalf("first insert = %v, %v", inserted, err)
	}
	if inserted, err := store.RecordGatewayRuntimeEvent(ctx, event); err != nil || inserted {
		t.Fatalf("duplicate insert = %v, %v", inserted, err)
	}
	got, err := store.LatestGatewayRuntimeEvent(ctx, event.EventType)
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != event.InstanceID {
		t.Fatalf("event = %+v", got)
	}
}

func TestGatewayRuntimeEventRejectsMissingIdentity(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if inserted, err := store.RecordGatewayRuntimeEvent(context.Background(), GatewayRuntimeEvent{EventType: "gateway.unclean_exit"}); err == nil || inserted {
		t.Fatalf("missing identity must be visible, inserted=%v err=%v", inserted, err)
	}
}

func TestGatewayRuntimeEventForInstanceDoesNotReturnAnotherDaemon(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, id := range []string{"gateway_old", "gateway_current"} {
		if inserted, err := store.RecordGatewayRuntimeEvent(ctx, GatewayRuntimeEvent{
			InstanceID: id, EventType: "prompt.snapshot.loaded",
		}); err != nil || !inserted {
			t.Fatalf("insert %s: inserted=%v err=%v", id, inserted, err)
		}
	}
	got, err := store.GatewayRuntimeEventForInstance(ctx, "gateway_current", "prompt.snapshot.loaded")
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != "gateway_current" {
		t.Fatalf("event=%+v", got)
	}
}
