package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

// TestSteerActiveRunSharedCore covers the core behind cross-endpoint steering:
// a continuation from the /v1/message path (IM/web) injects into the active
// run's steering channel via the SAME helper /v1/runs/steer uses, returns an
// accepted (not busy) response, and records a run.steered event. A full buffer
// reports back-pressure (ok=false) instead of dropping the guidance.
func TestSteerActiveRunSharedCore(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "openid-1", "WX User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Long task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "long coding task")
	if err != nil {
		t.Fatal(err)
	}

	steerCh := make(chan kernel.SteeringInput, 1)
	active := &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		TaskID:    task.ID,
		RunID:     run.ID,
		Channel:   "cli",
		Summary:   "Long task",
		StartedAt: time.Now(),
		Steer:     steerCh,
	}

	// A continuation arriving from weixin (a different channel than the run's)
	// must still reach the run.
	resp, ok := daemon.steerActiveRun(ctx, identity, active, api.MessageRequest{
		Channel: "weixin",
		Content: "also handle the retry path",
	})
	if !ok {
		t.Fatal("steerActiveRun returned ok=false on an empty buffer")
	}
	if !resp.Accepted {
		t.Fatalf("expected accepted response, got %+v", resp)
	}
	if resp.Turn == nil || resp.Turn.Status != "accepted" {
		t.Fatalf("expected turn status 'accepted', got %+v", resp.Turn)
	}
	select {
	case got := <-steerCh:
		if got.Content != "also handle the retry path" || got.ID == "" {
			t.Fatalf("steered input = %+v", got)
		}
	default:
		t.Fatal("guidance did not reach the steering channel")
	}

	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var steered *control.Event
	for i := range events {
		if events[i].Type == "run.steered" {
			steered = &events[i]
			break
		}
	}
	if steered == nil {
		t.Fatalf("run.steered event missing: %+v", events)
	}
	if strings.Contains(string(steered.Payload), "retry path") || !strings.Contains(string(steered.Payload), "steering_id") {
		t.Fatalf("run.steered payload = %s", steered.Payload)
	}

	// Fill the (size-1) buffer; the next continuation must report back-pressure
	// (ok=false) so the caller falls back to a busy reply — never a silent drop.
	steerCh <- kernel.SteeringInput{Content: "occupies the buffer"}
	if _, ok := daemon.steerActiveRun(ctx, identity, active, api.MessageRequest{Content: "overflow"}); ok {
		t.Fatal("expected ok=false on a full steering buffer (back-pressure), got ok=true")
	}

	// A nil steering channel (run without live steering) also reports ok=false.
	active.Steer = nil
	if _, ok := daemon.steerActiveRun(ctx, identity, active, api.MessageRequest{Content: "x"}); ok {
		t.Fatal("expected ok=false when the run has no steering channel")
	}
}
