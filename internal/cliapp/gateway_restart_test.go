package cliapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForGatewayIdleLeavesDeferredDaemonAvailableForHumanInput(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		active := 1
		if requests.Add(1) >= 2 {
			active = 0
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"state":"running","draining":false,"active_run_count":%d}`, active)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitForGatewayIdle(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 2 {
		t.Fatalf("status requests = %d; want polling until the active Run ends", requests.Load())
	}
}
