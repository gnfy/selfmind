package client

// Client-side presence reports process attachment, not keyboard activity. A
// person may watch a long-running agent without typing for many minutes; that
// must not make the daemon claim the terminal disappeared. Conversely, once
// the client process/stream is gone the heartbeat naturally expires.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"selfmind/internal/gateway/api"
)

func presenceCaptureServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"?active="+r.URL.Query().Get("active"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestPresenceBeatsAlwaysClaimLiveClientProcess(t *testing.T) {
	srv, seen := presenceCaptureServer(t)
	c := New(srv.URL, "")

	if err := c.PingPresence(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := c.openEventStream(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := seen()
	want := []string{
		"/v1/presence/ping?active=1",
		"/v1/events/stream?active=1",
	}
	if len(got) != len(want) {
		t.Fatalf("requests = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}
