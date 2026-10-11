package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

func TestWatchFallbackUsesRecordedRunState(t *testing.T) {
	for _, status := range []string{"waiting_user", "waiting_external", "interrupted", "running"} {
		t.Run(status, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/events/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					writeTestRunEvent(w, api.RunEvent{RunID: "parent", Type: "turn.completed", Durability: api.EventEphemeral})
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "/v1/tasks/current":
					writeJSONResp(w, map[string]any{"active_run": nil})
				case "/v1/runs/view":
					if r.URL.Query().Get("run_id") != "parent" {
						t.Errorf("wrong exact target: %s", r.URL.RawQuery)
					}
					id := "parent"
					if status == "running" {
						id = "exact-child"
					}
					writeJSONResp(w, api.RunView{RunID: id, Status: status, Summary: "durable result"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := New(srv.URL, "").WatchRun(ctx, "parent", nil)
			if err != nil || got.Status != status || got.Summary != "durable result" {
				t.Fatalf("observation=%+v error=%v", got, err)
			}
			if status == "running" && got.RunID != "exact-child" {
				t.Fatal("exact continuation was lost")
			}
		})
	}
}

func TestRunObservationDoesNotInventCompletion(t *testing.T) {
	for _, payload := range []string{`{}`, `{"outcome":{"summary":"ended"}}`, `broken`} {
		got, err := observedRunEvent(api.RunEvent{RunID: "target", Payload: []byte(payload)})
		if err == nil || got.Status == "done" {
			t.Fatalf("unrecorded outcome became completion: %+v %v", got, err)
		}
	}
	got, err := observedRunEvent(api.RunEvent{RunID: "target", Payload: []byte(`{"outcome":{"status":"waiting_user"},"final_answer":"Please confirm.\n\nNot dispatched."}`)})
	if err != nil || got.Status != "waiting_user" || got.FinalAnswer != "Please confirm.\n\nNot dispatched." {
		t.Fatalf("lost recorded state or final answer: %+v %v", got, err)
	}
}
