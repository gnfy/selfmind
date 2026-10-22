package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"selfmind/internal/gateway/api"
)

func TestWatchingOneOfSeveralRunsDoesNotEndOnMissingSingularStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active_runs":[{"run_id":"run_a"},{"run_id":"run_b"}]}`))
	}))
	defer server.Close()
	client := New(server.URL, "")
	for _, id := range []string{"run_a", "run_b", ""} {
		gone, ok := client.activeRunGone(context.Background(), api.MessageRequest{}, id)
		if !ok || gone {
			t.Fatalf("watch %q ended while active: %v %v", id, gone, ok)
		}
	}
	if gone, ok := client.activeRunGone(context.Background(), api.MessageRequest{}, "run_missing"); !ok || !gone {
		t.Fatal("unrelated Run kept exact watch alive")
	}
}
