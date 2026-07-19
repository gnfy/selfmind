package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

// TestRunSteerEndpoint covers the daemon side of client-mode mid-turn
// steering: guidance posted to /v1/runs/steer must land on the active run's
// steering channel and leave an auditable run.steered event; without an
// active run the daemon must refuse honestly (409), and a full buffer must
// surface as back-pressure (429) rather than dropped text.
func TestRunSteerEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	steer := func(text string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(api.RunSteerRequest{
			Platform:       "cli",
			PlatformUserID: "local",
			Channel:        "cli",
			Text:           text,
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/runs/steer", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := steer("   "); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := steer("focus on the tests"); rec.Code != http.StatusConflict {
		t.Fatalf("no-active-run status = %d, body = %s", rec.Code, rec.Body.String())
	}

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Steerable task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "long coding task")
	if err != nil {
		t.Fatal(err)
	}
	steerCh := make(chan kernel.SteeringInput, 2)
	if ok := daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		TaskID:    task.ID,
		RunID:     run.ID,
		Channel:   "cli",
		StartedAt: time.Now(),
		Steer:     steerCh,
	}); !ok {
		t.Fatal("could not register active run")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	rec := steer("please cover the unicode edge cases too")
	if rec.Code != http.StatusOK {
		t.Fatalf("steer status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp api.RunSteerResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted {
		t.Fatalf("steer response not accepted: %+v", resp)
	}
	select {
	case got := <-steerCh:
		if got.Content != "please cover the unicode edge cases too" || got.ID == "" || got.ContentHash == "" {
			t.Fatalf("steered input = %+v", got)
		}
	default:
		t.Fatal("guidance did not reach the run's steering channel")
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
	if steered.RunID != run.ID || strings.Contains(string(steered.Payload), "unicode edge cases") || !strings.Contains(string(steered.Payload), "steering_id") {
		t.Fatalf("run.steered event = %+v payload = %s", steered, steered.Payload)
	}

	// Fill the buffer; the next steer must report back-pressure, not block or drop.
	steerCh <- kernel.SteeringInput{Content: "queued-1"}
	steerCh <- kernel.SteeringInput{Content: "queued-2"}
	if rec := steer("overflow"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("full-buffer status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
