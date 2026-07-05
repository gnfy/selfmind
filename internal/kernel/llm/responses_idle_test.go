package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestResponsesStreamIdleWatchdogAborts verifies the SSE idle watchdog: a
// stream that sends an initial event then stalls (no further bytes, no EOF) is
// aborted with a retryable stream-idle error within roughly the idle timeout,
// so the retry loop can reconnect instead of hanging forever.
func TestResponsesStreamIdleWatchdogAborts(t *testing.T) {
	prev := configuredStreamIdle.Load()
	SetStreamIdleTimeout(80 * time.Millisecond)
	defer configuredStreamIdle.Store(prev)

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// One event, then stall — no EOF until the test releases us.
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	adapter := NewResponsesAdapter("token", server.URL, "gpt-test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := adapter.StreamChat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	start := time.Now()
	var idleErr error
	for ev := range ch {
		if ev.Err != nil {
			idleErr = ev.Err
			break
		}
	}
	elapsed := time.Since(start)

	if idleErr == nil {
		t.Fatal("expected a stream-idle error, got none")
	}
	if !strings.Contains(strings.ToLower(idleErr.Error()), "idle") {
		t.Fatalf("expected idle error, got %v", idleErr)
	}
	if !IsRetryableError(idleErr) {
		t.Fatalf("stream-idle error should be retryable: %v", idleErr)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("idle watchdog took too long to fire: %v", elapsed)
	}
}

// TestStreamIdleTimeoutConfigPrecedence checks the resolution order:
// SetStreamIdleTimeout override, then the built-in default when cleared.
func TestStreamIdleTimeoutConfigPrecedence(t *testing.T) {
	prev := configuredStreamIdle.Load()
	defer configuredStreamIdle.Store(prev)

	SetStreamIdleTimeout(0)
	if got := streamIdleTimeout(); got != DefaultStreamIdle {
		t.Fatalf("cleared override should yield default %v, got %v", DefaultStreamIdle, got)
	}
	SetStreamIdleTimeout(45 * time.Second)
	if got := streamIdleTimeout(); got != 45*time.Second {
		t.Fatalf("configured override should be honored, got %v", got)
	}
}
