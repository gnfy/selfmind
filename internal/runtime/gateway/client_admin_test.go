package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/gateway/httpapi"
)

// The daemon answers stop and status detail only to the person's own CLI on the
// local control socket (or a holder of the gateway token). The CLI therefore
// reaches a daemon on this host through that socket, and over TCP only when
// there is none: a daemon that predates it, or one on another host.

type recorded struct {
	mu       sync.Mutex
	requests []string
	headers  []http.Header
}

func (r *recorded) handler(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, req.Method+" "+req.URL.Path)
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func (r *recorded) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// serveControlSocket answers on the local control socket of a fresh data
// directory, the way a current daemon does.
func serveControlSocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	dataDir := shortDataDir(t)
	listener, err := listenLocalControl(LocalControlSocketPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return dataDir
}

func TestRequestShutdownReachesALocalDaemonOnItsControlSocket(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	socket, tcp := &recorded{}, &recorded{}
	dataDir := serveControlSocket(t, socket.handler(http.StatusAccepted, `{"accepted":true}`))
	server := httptest.NewServer(tcp.handler(http.StatusForbidden, `{"error":"over TCP"}`))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The runtime owner is not recorded, so the wait for it to exit ends at once.
	if err := RequestShutdown(ctx, StopOptions{URL: server.URL, DataDir: dataDir, Timeout: time.Second}); err != nil {
		t.Fatalf("shutdown through the control socket: %v", err)
	}
	if socket.count() != 1 || tcp.count() != 0 {
		t.Fatalf("socket requests=%d tcp requests=%d; a local daemon is stopped on its socket", socket.count(), tcp.count())
	}

	// A refusal on the socket is the daemon's answer; TCP would only be refused
	// again, and retrying there would hide why.
	refused, tcpAgain := &recorded{}, &recorded{}
	dataDir = serveControlSocket(t, refused.handler(http.StatusForbidden, `{"error":"this peer was refused"}`))
	server2 := httptest.NewServer(tcpAgain.handler(http.StatusAccepted, `{}`))
	defer server2.Close()
	err := RequestShutdown(ctx, StopOptions{URL: server2.URL, DataDir: dataDir, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refusal on the socket must be reported, got %v", err)
	}
	if tcpAgain.count() != 0 {
		t.Fatal("a refused request must not be retried over TCP")
	}
}

// The daemon's refusal does not say where its socket is; the CLI derives the path
// from the data directory it already holds and shows it to the person.
func TestARefusedShutdownShowsThePersonWhereTheSocketIs(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	refused := &recorded{}
	dataDir := serveControlSocket(t, refused.handler(http.StatusForbidden,
		`{"error":"stopping the daemon requires the person's own selfmind CLI on the daemon's local control socket"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := RequestShutdown(ctx, StopOptions{URL: "http://127.0.0.1:1", DataDir: dataDir, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), LocalControlSocketPath(dataDir)) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refused stop must show the socket path %s: %v", LocalControlSocketPath(dataDir), err)
	}
}

func TestRequestShutdownFallsBackToTCPOnlyWhenThereIsNoSocket(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	tcp := &recorded{}
	server := httptest.NewServer(tcp.handler(http.StatusAccepted, `{}`))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// A daemon that predates the control socket: nothing listens there.
	if err := RequestShutdown(ctx, StopOptions{URL: server.URL, DataDir: shortDataDir(t), Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if tcp.count() != 1 {
		t.Fatalf("tcp requests = %d, want the fallback to reach the old daemon", tcp.count())
	}
}

func TestRequestShutdownPresentsTheRestartCapabilityOnlyForAModelChange(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_GATEWAY_RESTART_CAPABILITY", "cap-1234")
	for reason, want := range map[string]string{
		"model_change:model_abc": "cap-1234",
		"MODEL_CHANGE:model_abc": "cap-1234",
		"":                       "",
		"service_reconcile":      "",
		"api shutdown":           "",
		"model_change:":          "",
	} {
		socket := &recorded{}
		dataDir := serveControlSocket(t, socket.handler(http.StatusAccepted, `{}`))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = RequestShutdown(ctx, StopOptions{URL: "http://127.0.0.1:1", DataDir: dataDir, Timeout: 200 * time.Millisecond, Reason: reason})
		cancel()
		if socket.count() == 0 {
			t.Fatalf("reason %q: the request never reached the socket", reason)
		}
		if got := socket.headers[0].Get(httpapi.RestartCapabilityHeader); got != want {
			t.Fatalf("reason %q: capability header = %q, want %q", reason, got, want)
		}
	}
	// Without the helper's environment nothing is sent.
	t.Setenv("SELF_GATEWAY_RESTART_CAPABILITY", "")
	socket := &recorded{}
	dataDir := serveControlSocket(t, socket.handler(http.StatusAccepted, `{}`))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = RequestShutdown(ctx, StopOptions{URL: "http://127.0.0.1:1", DataDir: dataDir, Timeout: 200 * time.Millisecond, Reason: "model_change:model_abc"})
	if socket.count() == 0 || socket.headers[0].Get(httpapi.RestartCapabilityHeader) != "" {
		t.Fatal("no capability may be sent when the environment holds none")
	}
}

func TestRequestStatusForPrefersTheControlSocket(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	socket, tcp := &recorded{}, &recorded{}
	dataDir := serveControlSocket(t, socket.handler(http.StatusOK, `{"state":"running","detail":"full"}`))
	server := httptest.NewServer(tcp.handler(http.StatusOK, `{"state":"running"}`))
	defer server.Close()

	data, code, err := RequestStatusFor(context.Background(), server.URL, dataDir)
	if err != nil || code != http.StatusOK || !strings.Contains(string(data), "full") {
		t.Fatalf("status through the socket: data=%s code=%d err=%v", data, code, err)
	}
	if tcp.count() != 0 {
		t.Fatal("a local daemon's status must come from its socket")
	}
	// A remote daemon, or no data directory, is TCP only.
	if _, _, err := RequestStatusFor(context.Background(), server.URL, ""); err != nil || tcp.count() != 1 {
		t.Fatalf("without a data directory the request goes over TCP: err=%v count=%d", err, tcp.count())
	}
	if _, _, err := RequestStatus(context.Background(), server.URL); err != nil || tcp.count() != 2 {
		t.Fatalf("RequestStatus is the TCP request: err=%v count=%d", err, tcp.count())
	}
}

func TestLocalGatewayURLNamesEveryFormOfAnAddressOnThisHost(t *testing.T) {
	for raw, want := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:8765": true, "LOCALHOST:8765": true, "[::1]:8765": true,
		":8765": true, "0.0.0.0:8765": true, "[::]:8765": true, "0.0.0.0": true, "::": true, "[::]": true,
		"http://127.0.0.1:8765": true, "http://localhost": true, "http://[::1]:8765": true,
		"http://:8765": true, "http://0.0.0.0:8765": true, "http://[::]:8765": true, "https://[::]:8443": true,
		"":                 true, // nothing named: the default address, which is loopback
		"203.0.113.9:8765": false, "10.0.0.5:8765": false, "gateway.example:8765": false, "gateway.example": false,
		"http://203.0.113.9:8765": false, "https://gateway.example": false, "http://127.0.0.1.evil.example:8765": false,
		"http://[2001:db8::1]:8765": false, "0.0.0.1:8765": false,
	} {
		if got := LocalGatewayURL(raw); got != want {
			t.Fatalf("LocalGatewayURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestOnlyALoopbackDaemonIsReachedOnTheControlSocket(t *testing.T) {
	for rawURL, want := range map[string]bool{
		"http://127.0.0.1:8765":   true,
		"http://localhost:8765":   true,
		"http://LOCALHOST:8765":   true,
		"http://[::1]:8765":       true,
		"http://:8765":            true, // binds every interface, still serves loopback
		"http://0.0.0.0:8765":     true,
		"http://[::]:8765":        true,
		"http://203.0.113.9:8765": false,
		"https://gateway.example": false,
		"http://10.0.0.5:8765":    false,
	} {
		if got := useLocalControl(rawURL, "/data"); got != want {
			t.Fatalf("useLocalControl(%q) = %v, want %v", rawURL, got, want)
		}
	}
	if useLocalControl("http://127.0.0.1:8765", "") {
		t.Fatal("without a data directory there is no socket to name")
	}
}

func TestRestartHelperCarriesItsCapabilityInItsEnvironmentOnly(t *testing.T) {
	cmd := restartHelperCommand("/usr/local/bin/selfmind", "/home/u/.selfmind/config.yaml", "model_abc", "cap-1234")
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{
		"SELF_GATEWAY_RESTART_REASON=model_change:model_abc",
		"SELF_GATEWAY_RESTART_CAPABILITY=cap-1234",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("restart helper environment misses %s", want)
		}
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "cap-1234") {
		t.Fatal("the capability must never appear in argv, which ps shows")
	}
	// The person's own CLI starts a helper that needs none.
	plain := restartHelperCommand("/usr/local/bin/selfmind", "", "model_abc", "")
	if strings.Contains(strings.Join(plain.Env, "\n"), "SELF_GATEWAY_RESTART_CAPABILITY=") && os.Getenv("SELF_GATEWAY_RESTART_CAPABILITY") == "" {
		t.Fatal("no capability variable may be set when there is none to pass")
	}
}
