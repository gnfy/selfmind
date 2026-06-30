package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// healthServer stands up a fake gateway that becomes healthy after `after`
// successful-once flips; it lets us exercise the readiness wait without a real
// daemon process.
func healthServer(t *testing.T, healthyAfter int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&hits, 1)
		if int(n) <= healthyAfter {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestPingHealth(t *testing.T) {
	srv, _ := healthServer(t, 0)
	if err := pingHealth(context.Background(), srv.URL); err != nil {
		t.Fatalf("pingHealth healthy: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer down.Close()
	if err := pingHealth(context.Background(), down.URL); err == nil {
		t.Fatal("pingHealth should fail on non-200")
	}
}

func TestWaitHealthyBecomesReady(t *testing.T) {
	// Unhealthy for the first two probes, then OK — waitHealthy must keep polling.
	srv, hits := healthServer(t, 2)
	start := time.Now()
	if err := waitHealthy(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if atomic.LoadInt32(hits) < 3 {
		t.Fatalf("expected at least 3 probes, got %d", atomic.LoadInt32(hits))
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("waitHealthy took too long: %v", time.Since(start))
	}
}

func TestWaitHealthyTimeout(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := waitHealthy(context.Background(), down.URL, 300*time.Millisecond); err == nil {
		t.Fatal("waitHealthy should time out against a never-healthy server")
	}
}

func TestWaitHealthyContextCancel(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitHealthy(ctx, down.URL, 5*time.Second); err == nil {
		t.Fatal("waitHealthy should return when ctx is cancelled")
	}
}

// TestEnsureRunningUsesExistingDaemon verifies the fast path: when a live PID
// record exists and /health answers, EnsureRunning returns it WITHOUT spawning a
// new process (Started=false).
func TestEnsureRunningUsesExistingDaemon(t *testing.T) {
	srv, _ := healthServer(t, 0)

	dataDir := t.TempDir()
	// Minimal config pointing storage at our temp dir so resolveDataDir lands
	// the gateway runtime files where we write the fake record.
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("storage:\n  data_dir: "+dataDir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Point the client URL resolver at our fake healthy server.
	t.Setenv("SELF_GATEWAY_URL", srv.URL)

	// Write a live running record (our own PID is alive) so RunningRecord() is true.
	m := NewManager(dataDir, "127.0.0.1:8765")
	if err := m.WriteStatus("running", "default", ""); err != nil {
		t.Fatal(err)
	}

	res, err := EnsureRunning(context.Background(), EnsureOptions{ConfigPath: cfgPath, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if res.Started {
		t.Fatal("EnsureRunning must not spawn when a healthy daemon already runs")
	}
	if res.URL != srv.URL {
		t.Fatalf("URL = %q, want %q", res.URL, srv.URL)
	}
	if res.Record.PID != os.Getpid() {
		t.Fatalf("record PID = %d, want %d", res.Record.PID, os.Getpid())
	}
}
