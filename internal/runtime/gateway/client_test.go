package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDetachedRunArgsDoNotLeakLifecycleFlags(t *testing.T) {
	want := []string{"gateway", "run"}
	if got := detachedRunArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("detachedRunArgs() = %v, want %v", got, want)
	}
}

func TestRequestShutdownCanAbortCancellableSafeBoundaryWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	err := RequestShutdown(context.Background(), StopOptions{
		URL: server.URL, Timeout: time.Second, WaitForSafeBoundary: true,
		Abort: func() bool { return true },
	})
	if !errors.Is(err, ErrShutdownAborted) {
		t.Fatalf("RequestShutdown error = %v; want ErrShutdownAborted", err)
	}
}

func TestServiceReconcileReportsDeferredWhenGatewayResumesAfterBoundedDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/gateway/shutdown":
			w.WriteHeader(http.StatusAccepted)
		case "/v1/gateway/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"running","draining":false,"active_run_count":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	err := RequestShutdown(context.Background(), StopOptions{
		URL: server.URL, DataDir: t.TempDir(), Timeout: time.Second,
		Reason: "service_reconcile", WaitForSafeBoundary: true,
	})
	if !errors.Is(err, ErrShutdownDeferred) {
		t.Fatalf("service reconciliation error = %v, want ErrShutdownDeferred", err)
	}
}

// TestPickDaemonExecutable pins the daemon-binary resolution order and the
// after-upgrade fallback: a current executable whose path no longer exists
// (npm swaps the package directory out from under a running process) must
// fall back to `selfmind` on PATH instead of fork/exec'ing a deleted path.
func TestPickDaemonExecutable(t *testing.T) {
	// Explicit override always wins, even over an existing current executable.
	if got, err := pickDaemonExecutable("/opt/custom/selfmind", "/whatever", nil); err != nil || got != "/opt/custom/selfmind" {
		t.Fatalf("override: got %q err %v, want the override", got, err)
	}

	// Existing current executable is used as-is.
	current := filepath.Join(t.TempDir(), "selfmind-current")
	if err := os.WriteFile(current, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := pickDaemonExecutable("", current, nil); err != nil || got != current {
		t.Fatalf("existing current: got %q err %v, want %q", got, err, current)
	}

	// Deleted current executable falls back to PATH.
	binDir := t.TempDir()
	fromPath := filepath.Join(binDir, "selfmind")
	if err := os.WriteFile(fromPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	if got, err := pickDaemonExecutable("", filepath.Join(binDir, "gone-after-upgrade"), nil); err != nil || got != fromPath {
		t.Fatalf("deleted current: got %q err %v, want PATH fallback %q", got, err, fromPath)
	}

	// Deleted current + nothing on PATH is an actionable error.
	t.Setenv("PATH", t.TempDir())
	if _, err := pickDaemonExecutable("", "/nonexistent/selfmind", nil); err == nil {
		t.Fatal("deleted current with empty PATH must error")
	}
}

func TestMergeRestartEnvironmentUsesOnlyCurrentProxy(t *testing.T) {
	sep := string(os.PathListSeparator)
	current := []string{"PATH=" + strings.Join([]string{"/usr/bin"}, sep), "HTTP_PROXY=http://new-proxy:8080", "NO_PROXY="}
	previous := []string{
		"PATH=" + strings.Join([]string{"/home/test/.local/bin", "/usr/bin", "/home/test/google-cloud-sdk/bin"}, sep),
		"HTTP_PROXY=http://old-proxy:8080",
		"http_proxy=http://old-proxy:8080",
		"https_proxy=http://old-proxy:8080",
		"NO_PROXY=localhost",
		"SECRET_TOKEN=must-not-copy",
	}
	got := mergeRestartEnvironment(current, previous)
	joined := strings.Join(got, "\n")

	wantPath := "PATH=" + strings.Join([]string{"/usr/bin", "/home/test/.local/bin", "/home/test/google-cloud-sdk/bin"}, sep)
	if !strings.Contains(joined, wantPath) {
		t.Fatalf("current PATH must lead while old-only tool directories survive: %v", got)
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://new-proxy:8080") {
		t.Fatalf("current proxy must win: %v", got)
	}
	if strings.Contains(joined, "HTTP_PROXY=http://old-proxy:8080") {
		t.Fatalf("old proxy must not override current proxy: %v", got)
	}
	if strings.Contains(joined, "http_proxy=http://old-proxy:8080") {
		t.Fatalf("old case variant must not override a current proxy: %v", got)
	}
	if strings.Contains(joined, "https_proxy=http://old-proxy:8080") {
		t.Fatalf("a proxy missing from the current environment must not survive restart: %v", got)
	}
	if strings.Contains(joined, "NO_PROXY=localhost") {
		t.Fatalf("an explicitly empty current value must clear the old value: %v", got)
	}
	if strings.Contains(joined, "SECRET_TOKEN") {
		t.Fatalf("non-proxy environment must never be copied: %v", got)
	}
}

func TestMergeRestartEnvironmentDoesNotResurrectOldProxy(t *testing.T) {
	got := mergeRestartEnvironment(
		[]string{"PATH=/usr/bin"},
		[]string{"HTTP_PROXY=http://proxy:8080", "http_proxy=http://proxy:8080"},
	)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "PROXY=") || strings.Contains(joined, "proxy=") {
		t.Fatalf("old proxy settings must not survive an environment-less restart: %v", got)
	}
}

func TestRestartEnvironmentFromBlockFiltersKeys(t *testing.T) {
	block := []byte("PATH=/usr/bin\x00HTTPS_PROXY=http://proxy:8080\x00no_proxy=localhost\x00TOKEN=secret\x00")
	got := restartEnvironmentFromBlock(block)
	want := []string{"PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restartEnvironmentFromBlock() = %v, want %v", got, want)
	}
}
