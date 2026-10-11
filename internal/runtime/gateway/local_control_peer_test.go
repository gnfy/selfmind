package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/platform/config"
	platformlog "selfmind/internal/platform/log"
	"selfmind/internal/platform/peercred"
)

// controlSocketFixture serves a real httpapi server on a real local control
// socket through the daemon's own connection plumbing.
type controlSocketFixture struct {
	dataDir string
	control *localControlRuntime
}

func newControlSocketFixture(t *testing.T) *controlSocketFixture {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	dataDir := shortDataDir(t)
	control := startLocalControl(context.Background(), dataDir, &config.Config{}, "127.0.0.1:0")
	t.Cleanup(control.stop)
	listener, err := listenLocalControl(control.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &httpapi.Server{Control: controltest.NewStore(t), DefaultTenantID: "default"}
	httpServer := &http.Server{Handler: server.Handler(), ConnContext: control.connContext}
	go httpServer.Serve(listener)
	t.Cleanup(func() { _ = httpServer.Close() })
	return &controlSocketFixture{dataDir: dataDir, control: control}
}

func (f *controlSocketFixture) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, "http://control"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := LocalControlClient(f.dataDir).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func requirePeerCredentials(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("peer credentials are implemented for darwin and linux")
	}
}

// A probe that connects and leaves, as the CLI's own readiness check does, asks
// for nothing and must not be judged; traffic that does not claim CLI identity
// is never judged either; the first CLI claim is judged exactly once.
func TestTheDaemonJudgesAPeerOnlyWhenARequestClaimsCLIIdentity(t *testing.T) {
	requirePeerCredentials(t)
	fixture := newControlSocketFixture(t)
	var judged atomic.Int32
	fixture.control.judge = func(net.Conn) peercred.Verdict {
		judged.Add(1)
		return peercred.Verdict{Verified: true, UID: os.Getuid(), PID: 4242}
	}

	probe, err := net.Dial("unix", fixture.control.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	if code, _ := fixture.do(t, http.MethodGet, "/health", ""); code != http.StatusOK {
		t.Fatalf("health status %d", code)
	}
	if code, _ := fixture.do(t, http.MethodPost, "/v1/message", `{"platform":"http","platform_user_id":"integrator","content":"/id"}`); code != http.StatusOK {
		t.Fatalf("an HTTP identity status %d", code)
	}
	if judged.Load() != 0 {
		t.Fatalf("a probe, a health check and an HTTP identity must not be judged, got %d judgements", judged.Load())
	}
	if code, body := fixture.do(t, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/id"}`); code != http.StatusOK {
		t.Fatalf("a verified CLI claim status %d %s", code, body)
	}
	if judged.Load() != 1 {
		t.Fatalf("the CLI claim must be judged once, got %d", judged.Load())
	}
}

type warnSink struct {
	mu   sync.Mutex
	text strings.Builder
}

func (s *warnSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.Write(p)
}

func (s *warnSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// The daemon judges a connection whenever a request needs to know who is on it,
// among them the restart helper stopping the daemon and a liveness read. Only a
// request that asked for something its peer may not have is a warning.
func TestAPeerIsWarnedAboutOnlyWhenItsRequestIsRefused(t *testing.T) {
	requirePeerCredentials(t)
	sink := &warnSink{}
	platformlog.Init(platformlog.Options{Level: "warn", Output: sink})
	t.Cleanup(func() { platformlog.Init(platformlog.Options{}) })
	fixture := newControlSocketFixture(t)

	// This test process is the daemon here, so every connection of its own is a
	// refused peer.
	if code, body := fixture.do(t, http.MethodGet, "/v1/gateway/status", ""); code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	if strings.Contains(sink.String(), "refused") {
		t.Fatalf("a liveness read from a refused peer must not warn:\n%s", sink.String())
	}
	if code, _ := fixture.do(t, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/mode full-auto"}`); code != http.StatusForbidden {
		t.Fatalf("a CLI claim from a refused peer must be refused, got %d", code)
	}
	if strings.Count(sink.String(), "a peer claiming CLI identity was refused") != 1 {
		t.Fatalf("the refused claim must warn exactly once:\n%s", sink.String())
	}
}

// The whole chain with the real verifier: this test process plays the daemon,
// so its own connection is refused as the daemon; with the daemon identity
// pointed at another process the same connection is the person's CLI.
func TestTheRealVerifierRefusesTheDaemonItselfAndAcceptsAnotherProcess(t *testing.T) {
	requirePeerCredentials(t)
	fixture := newControlSocketFixture(t)
	verdict := func() api.LocalControlPeer {
		code, body := fixture.do(t, http.MethodGet, "/v1/gateway/status", "")
		if code != http.StatusOK {
			t.Fatalf("status %d %s", code, body)
		}
		var status api.GatewayStatusResponse
		if err := json.Unmarshal([]byte(body), &status); err != nil {
			t.Fatal(err)
		}
		return status.LocalControl
	}
	if got := verdict(); !got.Socket || got.Verified || !strings.Contains(got.Refusal, "daemon") || got.PID != os.Getpid() {
		t.Fatalf("the daemon's own process must be refused: %+v", got)
	}
	if code, body := fixture.do(t, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/mode full-auto"}`); code != http.StatusForbidden || !strings.Contains(body, "refused") {
		t.Fatalf("a CLI claim from the daemon's own process must be refused: %d %s", code, body)
	}

	// The same connection, judged by a daemon that is an unrelated process: a
	// sleeper this process started is not an ancestor of it.
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	fixture.control.verifier.DaemonPID = other.Process.Pid
	fixture.control.judge = fixture.control.verifier.Check
	if got := verdict(); !got.Verified || got.PID != os.Getpid() {
		t.Fatalf("a process outside the daemon's tree must be the person's CLI: %+v", got)
	}
	if code, body := fixture.do(t, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/id"}`); code != http.StatusOK {
		t.Fatalf("the verified person's CLI claim must be served: %d %s", code, body)
	}
}
