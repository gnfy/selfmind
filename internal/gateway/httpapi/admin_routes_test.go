package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
)

// Stopping the daemon and reading what it is doing are the person's own
// controls. With no gateway token configured, the TCP listener is open to every
// local process, including a confined tool that reached it; these routes then
// answer only the verified local CLI. A configured token still opens them to the
// remote operator who holds it, and `/health` stays open to everyone.

const adminSocketPath = "/home/u/.selfmind/data/gateway/control.sock"

func adminServer(stopped chan<- struct{}) *Server {
	return &Server{
		DrainTimeout: time.Millisecond,
		ShutdownFunc: func() {
			select {
			case stopped <- struct{}{}:
			default:
			}
		},
		RuntimeStatusFunc: func() api.GatewayRuntimeInfo {
			return api.GatewayRuntimeInfo{
				PID: 4242, Addr: "127.0.0.1:8765", State: "running",
				LocalControlSocket:    adminSocketPath,
				DataDir:               "/home/u/.selfmind/data",
				RuntimeDir:            "/home/u/.selfmind/data/gateway",
				ConfigPath:            "/home/u/.selfmind/config.yaml",
				ModelRouteFingerprint: "route-fingerprint",
			}
		},
	}
}

func TestShutdownAnswersOnlyTheLocalCLIOrAHolderOfTheGatewayToken(t *testing.T) {
	type attempt struct {
		name    string
		peer    LocalPeer
		token   string
		headers []string
		want    int
		stops   bool
		message string
	}
	attempts := []attempt{
		{name: "tcp, no token configured", peer: tcpPeer, want: http.StatusForbidden,
			message: "local control socket"},
		{name: "tcp, no token configured, a gateway token sent anyway", peer: tcpPeer, want: http.StatusForbidden,
			headers: []string{"Authorization", "Bearer guessed"}, message: "gateway token"},
		{name: "socket, refused peer, no token configured", peer: refusedPeer, want: http.StatusForbidden,
			message: refusedPeer.Refusal},
		{name: "tcp, token configured, none sent", peer: tcpPeer, token: "operator", want: http.StatusUnauthorized},
		{name: "tcp, token configured, wrong token", peer: tcpPeer, token: "operator", want: http.StatusUnauthorized,
			headers: []string{"Authorization", "Bearer guessed"}},
		{name: "tcp, token configured and sent", peer: tcpPeer, token: "operator", want: http.StatusAccepted, stops: true,
			headers: []string{"Authorization", "Bearer operator"}},
		{name: "socket, verified CLI, no token configured", peer: verifiedPeer, want: http.StatusAccepted, stops: true},
		{name: "socket, verified CLI, token configured and sent", peer: verifiedPeer, token: "operator", want: http.StatusAccepted, stops: true,
			headers: []string{"Authorization", "Bearer operator"}},
		{name: "socket, refused peer holding the token", peer: refusedPeer, token: "operator", want: http.StatusAccepted, stops: true,
			headers: []string{"X-SelfMind-Token", "operator"}},
	}
	for _, tc := range attempts {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SELF_GATEWAY_TOKEN", tc.token)
			t.Setenv("SELF_DAEMON_TOKEN", "")
			stopped := make(chan struct{}, 1)
			daemon := adminServer(stopped)
			recorder := serveAs(t, daemon, tc.peer, http.MethodPost, "/v1/gateway/shutdown", `{}`, tc.headers...)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.message != "" && !strings.Contains(recorder.Body.String(), tc.message) {
				t.Fatalf("the refusal must say %q: %s", tc.message, recorder.Body.String())
			}
			// Where the socket is stays out of an answer to a caller that has not
			// proved it may know: the CLI derives the path and shows it to the person.
			if tc.want >= http.StatusBadRequest && (strings.Contains(recorder.Body.String(), adminSocketPath) || strings.Contains(recorder.Body.String(), "/home/u/")) {
				t.Fatalf("a refusal must not disclose the socket path: %s", recorder.Body.String())
			}
			select {
			case <-stopped:
				if !tc.stops {
					t.Fatal("a refused shutdown request stopped the daemon")
				}
			case <-time.After(300 * time.Millisecond):
				if tc.stops {
					t.Fatal("an authorized shutdown request never stopped the daemon")
				}
			}
			if !tc.stops && daemon.IsDraining() {
				t.Fatal("a refused shutdown request left the daemon draining")
			}
		})
	}
}

// The model-change restart helper is a descendant of the daemon, so the daemon
// never takes it for the person's CLI. It proves it was started for this very
// change with a secret only the two share, and gets nothing else with it.
func TestRestartHelperMayStopTheDaemonOnlyForItsOwnModelChange(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	service, _ := testModelChangeService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-after-restart"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{Candidate: candidate, Source: "test", RequireConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	reason := "model_change:" + prepared.Change.ID
	body := func(reason string) string {
		data, _ := json.Marshal(map[string]string{"reason": reason})
		return string(data)
	}
	attempts := []struct {
		name       string
		capability string // what the server holds
		sent       string
		reason     string
		want       int
	}{
		{"the helper with the capability, for its model change", "cap-1234", "cap-1234", reason, http.StatusAccepted},
		{"a wrong capability", "cap-1234", "cap-0000", reason, http.StatusForbidden},
		{"no capability", "cap-1234", "", reason, http.StatusForbidden},
		{"the right capability for a different reason", "cap-1234", "cap-1234", "api shutdown", http.StatusForbidden},
		{"the right capability for a service reconcile", "cap-1234", "cap-1234", api.ShutdownReasonServiceReconcile, http.StatusForbidden},
		{"an empty capability on both sides", "", "", reason, http.StatusForbidden},
	}
	for _, tc := range attempts {
		t.Run(tc.name, func(t *testing.T) {
			stopped := make(chan struct{}, 1)
			daemon := adminServer(stopped)
			daemon.ModelChanges = service
			daemon.RestartCapability = tc.capability
			var headers []string
			if tc.sent != "" {
				headers = []string{"X-SelfMind-Restart-Capability", tc.sent}
			}
			recorder := serveAs(t, daemon, refusedPeer, http.MethodPost, "/v1/gateway/shutdown", body(tc.reason), headers...)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
			select {
			case <-stopped:
				if tc.want != http.StatusAccepted {
					t.Fatal("a refused request stopped the daemon")
				}
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

func TestStatusDetailAnswersOnlyTheLocalCLIOrAHolderOfTheGatewayToken(t *testing.T) {
	attempts := []struct {
		name    string
		peer    LocalPeer
		token   string
		headers []string
		detail  bool
	}{
		{name: "tcp, no token configured", peer: tcpPeer},
		{name: "tcp, no token configured, a token sent anyway", peer: tcpPeer, headers: []string{"Authorization", "Bearer guessed"}},
		{name: "socket, refused peer", peer: refusedPeer},
		{name: "tcp, token configured and sent", peer: tcpPeer, token: "operator", headers: []string{"Authorization", "Bearer operator"}, detail: true},
		{name: "socket, verified CLI", peer: verifiedPeer, detail: true},
	}
	for _, tc := range attempts {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SELF_GATEWAY_TOKEN", tc.token)
			t.Setenv("SELF_DAEMON_TOKEN", "")
			daemon := adminServer(nil)
			if !daemon.coordinator().beginActive("person_a", &activeRun{
				PersonID: "person_a", RunID: "run_secret", Summary: "rotate the production database password", StartedAt: time.Now(),
				Cancel: func() {},
			}) {
				t.Fatal("active run registration failed")
			}
			recorder := serveAs(t, daemon, tc.peer, http.MethodGet, "/v1/gateway/status", "", tc.headers...)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			var status api.GatewayStatusResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			// Liveness never needs the detail: state, draining and how much work
			// is running are what restart and doctor poll for.
			if status.State != "running" || status.Draining || status.ActiveRunCount != 1 || status.Runtime.State != "running" {
				t.Fatalf("liveness fields must always answer: %+v", status)
			}
			// A withheld answer says it is withheld, so a client can tell it from a
			// daemon with nothing to report.
			if status.LivenessOnly == tc.detail {
				t.Fatalf("liveness_only = %v for a caller entitled to detail = %v", status.LivenessOnly, tc.detail)
			}
			body := recorder.Body.String()
			for _, secret := range []string{
				"run_secret", "rotate the production database password", "person_a", adminSocketPath,
				"/home/u/.selfmind", "route-fingerprint",
			} {
				if got := strings.Contains(body, secret); got != tc.detail {
					t.Fatalf("%q present = %v, want %v in:\n%s", secret, got, tc.detail, body)
				}
			}
			if tc.detail && len(status.ActiveRuns) != 1 {
				t.Fatalf("the verified CLI and token holders see the active runs: %+v", status.ActiveRuns)
			}
			if !tc.detail && len(status.ActiveRuns) != 0 {
				t.Fatalf("active runs must be withheld: %+v", status.ActiveRuns)
			}
		})
	}
}

// /health answers every caller: it says only that something is listening.
func TestHealthStaysOpen(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	daemon := adminServer(nil)
	daemon.ListenAddr = "127.0.0.1:8765"
	for _, peer := range []LocalPeer{tcpPeer, refusedPeer, verifiedPeer} {
		recorder := serveAs(t, daemon, peer, http.MethodGet, "/health", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("health status = %d for %+v", recorder.Code, peer)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://rebound.example:8765/health", nil)
	recorder := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("health must answer any Host: %d", recorder.Code)
	}
}

// A page that points its own name at 127.0.0.1 reaches the daemon with that name
// in the Host header and, after the rebinding, no Origin: same-origin to the
// browser. A loopback-only daemon accepts only its own addresses.
func TestLoopbackDaemonRefusesAHostThatIsNotLoopback(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	daemon := adminServer(nil)
	daemon.ListenAddr = "127.0.0.1:8765"
	get := func(host string, peer LocalPeer) int {
		request := httptest.NewRequest(http.MethodGet, "/v1/gateway/status", nil)
		request.Host = host
		request = request.WithContext(WithLocalPeer(request.Context(), peer))
		recorder := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	for _, host := range []string{
		"127.0.0.1:8765", "localhost:8765", "LOCALHOST:8765", "[::1]:8765", "127.0.0.1", "localhost", "[::1]",
	} {
		if code := get(host, tcpPeer); code != http.StatusOK {
			t.Fatalf("Host %q must be accepted, got %d", host, code)
		}
	}
	for _, host := range []string{
		"rebind.attacker.example:8765", "rebind.attacker.example", "127.0.0.1.attacker.example:8765",
		"localhost.attacker.example:8765", "0.0.0.0:8765", "192.168.1.20:8765", "127.0.0.1:9999", "localhost:80",
		"[::1]:9999", "attacker.example@127.0.0.1:8765",
	} {
		if code := get(host, tcpPeer); code != http.StatusForbidden {
			t.Fatalf("Host %q must be refused, got %d", host, code)
		}
	}
	// An HTTP/1.0 client may send no Host; a browser always does, so the
	// rebinding attack cannot.
	if code := get("", tcpPeer); code != http.StatusOK {
		t.Fatalf("a request without a Host header must be accepted, got %d", code)
	}
	// The socket has no browser on the other end: whatever name the CLI's URL
	// carries is only a label.
	if code := get("anything.example", verifiedPeer); code != http.StatusOK {
		t.Fatalf("a socket request must not be judged by its Host, got %d", code)
	}
	// The refusal tells the operator what the daemon expects.
	request := httptest.NewRequest(http.MethodGet, "/v1/gateway/status", nil)
	request.Host = "rebind.attacker.example:8765"
	recorder := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "127.0.0.1:8765") {
		t.Fatalf("the refusal must name the address the daemon expects: %s", recorder.Body.String())
	}

	// The constraint that must change the result: a daemon bound beyond loopback
	// cannot know its own names, so it leaves the Host to its token.
	for _, addr := range []string{"0.0.0.0:8765", ":8765", "192.168.1.20:8765", ""} {
		daemon.ListenAddr = addr
		if code := get("rebind.attacker.example:8765", tcpPeer); code != http.StatusOK {
			t.Fatalf("listen address %q must leave the Host unchecked, got %d", addr, code)
		}
	}
	daemon.ListenAddr = "localhost:8765"
	if code := get("rebind.attacker.example:8765", tcpPeer); code != http.StatusForbidden {
		t.Fatalf("a localhost listener is loopback-only too, got %d", code)
	}
	daemon.ListenAddr = "[::1]:8765"
	if code := get("[::1]:8765", tcpPeer); code != http.StatusOK {
		t.Fatalf("an IPv6 loopback listener accepts [::1], got %d", code)
	}
}
