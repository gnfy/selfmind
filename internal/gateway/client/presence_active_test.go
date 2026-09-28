package client

// Client-side presence reports process attachment, not keyboard activity. A
// person may watch a long-running agent without typing for many minutes; that
// must not make the daemon claim the terminal disappeared. Conversely, once
// the client process/stream is gone the heartbeat naturally expires.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	resp, err := c.openEventStream(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local"}, "", 0)
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

// A terminal subscribes as its session, a run watcher names the run it
// attaches to, and a heartbeat reports the other-session detail events the
// terminal dropped, so the daemon can count audience misses.
func TestClientNamesItsSessionAttachedRunAndDroppedEvents(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Path+"?"+r.URL.Query().Encode())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "")
	for _, attach := range []string{"", "run-a"} {
		resp, err := c.openEventStream(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-b"}, attach, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if err := c.pingPresence(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 3 ||
		!strings.Contains(queries[0], "session=session-b") || strings.Contains(queries[0], "run=") ||
		!strings.Contains(queries[1], "run=run-a") ||
		!strings.Contains(queries[2], "foreign_session_events=3") {
		t.Fatalf("requests = %v", queries)
	}
}
