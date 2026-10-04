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
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
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
	store := controltest.NewStore(t)

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	steer := func(runID, channel, text string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(api.RunSteerRequest{
			Platform:       "cli",
			PlatformUserID: "local",
			RunID:          runID,
			Channel:        channel,
			Text:           text,
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/runs/steer", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := steer("", "cli", "   "); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := steer("no-active-run", "cli", "focus on the tests"); rec.Code != http.StatusConflict {
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

	if rec := steer("", "cli", "unbound guidance"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := steer(run.ID, "", "unbound guidance"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-channel status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := steer("another-run", "cli", "wrong run"); rec.Code != http.StatusConflict {
		t.Fatalf("wrong-run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := steer(run.ID, "other-session", "cross-session guidance without source scope"); rec.Code != http.StatusConflict {
		t.Fatalf("unsafe cross-session steer status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := steer(run.ID, "cli", "please cover the unicode edge cases too")
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
	if rec := steer(run.ID, "cli", "overflow"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("full-buffer status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// Guidance posted to /v1/runs/steer is for that exact run, so it carries the
// run's roots into separate work. The endpoint passed none, and work queued
// from it lost the run's --add-dir root.
func TestRunSteerEndpointKeepsTheRunsRoots(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Steerable task", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	roots := []executionenv.RootBinding{
		{Path: "/work/app", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace},
		{Path: "/data/shared", Role: executionenv.RootRoleAdditional, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir},
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "long coding task", control.StartRunOptions{ExecutionRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if ok := daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "cli", ExecutionRoots: roots, StartedAt: time.Now(), Steer: make(chan kernel.SteeringInput, 1),
	}); !ok {
		t.Fatal("could not register active run")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	body, _ := json.Marshal(api.RunSteerRequest{Platform: "cli", PlatformUserID: "local", RunID: run.ID, Channel: "cli", Text: "separately, bump the changelog"})
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/steer", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("steer status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("steering rows = %+v err=%v", rows, err)
	}
	queued, err := store.QueueSteeringAsIndependent(ctx, identity.TenantID, identity.PersonID, run.ID, rows[0].ID)
	if err != nil || queued == nil {
		t.Fatalf("queued=%+v err=%v", queued, err)
	}
	var paths []string
	for _, root := range queued.ExecutionRoots {
		paths = append(paths, root.Path)
	}
	if strings.Join(paths, ",") != "/work/app,/data/shared" {
		t.Fatalf("queued work roots = %v, want the run's roots with its --add-dir", paths)
	}
}
