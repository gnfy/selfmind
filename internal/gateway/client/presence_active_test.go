package client

// Client-side presence honesty: the presence ping and the unified event stream carry
// active=0|1 computed from the age of the last user input, so an open TUI at
// a vacated desk stops claiming attachment (the daemon then lets presence
// expire and pushes route to the preferred IM again).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

// presenceCaptureServer records the active query param of every presence ping
// and event-stream connection it serves.
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
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

func TestPresenceBeatsCarryInputDerivedActiveFlag(t *testing.T) {
	srv, seen := presenceCaptureServer(t)
	c := New(srv.URL, "")

	lastInput := time.Now()
	c.IdleTimeout = time.Minute
	c.LastInput = func() time.Time { return lastInput }

	// Fresh input: both surfaces claim active=1.
	if err := c.PingPresence(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := c.openEventStream(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Stale input (older than the timeout): both surfaces stop claiming.
	lastInput = time.Now().Add(-2 * time.Minute)
	if err := c.PingPresence(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err = c.openEventStream(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := seen()
	want := []string{
		"/v1/presence/ping?active=1",
		"/v1/events/stream?active=1",
		"/v1/presence/ping?active=0",
		"/v1/events/stream?active=0",
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

// TestPresenceActiveFailsOpen: missing wiring or a disabled (0) timeout must
// keep the old always-attached behavior — a config gap never silently mutes a
// live terminal's presence.
func TestPresenceActiveFailsOpen(t *testing.T) {
	c := New("http://127.0.0.1:0", "")
	if !c.presenceActive() {
		t.Fatal("no wiring: must claim active")
	}
	c.IdleTimeout = 0
	c.LastInput = func() time.Time { return time.Now().Add(-time.Hour) }
	if !c.presenceActive() {
		t.Fatal("zero timeout disables idle detection")
	}
	c.IdleTimeout = time.Minute
	if c.presenceActive() {
		t.Fatal("stale input with a timeout must read as inactive")
	}
	c.LastInput = func() time.Time { return time.Time{} }
	if !c.presenceActive() {
		t.Fatal("a zero last-input time fails open to active")
	}
}

// TestInputTrackerSeedsAndTouches: the tracker starts "just used" (launching
// the TUI is input) and Touch refreshes it.
func TestInputTrackerSeedsAndTouches(t *testing.T) {
	tracker := NewInputTracker()
	if tracker.Last().IsZero() || time.Since(tracker.Last()) > time.Minute {
		t.Fatalf("tracker must seed to now, got %v", tracker.Last())
	}
	before := tracker.Last()
	time.Sleep(2 * time.Millisecond)
	tracker.Touch()
	if !tracker.Last().After(before) {
		t.Fatal("Touch must advance the last-input time")
	}
}
