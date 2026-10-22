package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// Only the person's own CLI, verified by the daemon from kernel peer
// credentials on the local control socket, may act with CLI identity. These
// tests drive the real handler with the three transports a request can have:
// TCP (no verdict), the socket with a refused peer, and the socket with the
// verified person.

var (
	tcpPeer      = LocalPeer{}
	refusedPeer  = LocalPeer{Socket: true, Refusal: "peer is the daemon or a process it started"}
	verifiedPeer = LocalPeer{Socket: true, Verified: true, UID: os.Getuid(), PID: 4242}
)

// asLocalCLI marks a test request as coming from the person's verified CLI.
func asLocalCLI(r *http.Request) *http.Request {
	return r.WithContext(WithLocalPeer(r.Context(), verifiedPeer))
}

// newLocalCLIServer serves handler as the daemon serves its local control
// socket to the verified person: every connection carries the verdict.
func newLocalCLIServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return WithLocalPeer(ctx, verifiedPeer)
	}
	server.Start()
	return server
}

func serveAs(t *testing.T, daemon *Server, peer LocalPeer, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	request.RemoteAddr = "127.0.0.1:4100"
	// A stream that was wrongly admitted ends with this deadline instead of
	// hanging the test.
	ctx, cancel := context.WithTimeout(WithLocalPeer(request.Context(), peer), 3*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	return recorder
}

func approvalMode(t *testing.T, store *control.Store, identity *control.IdentityContext) string {
	t.Helper()
	mode, err := store.GetPersonSetting(context.Background(), identity.TenantID, identity.PersonID, personSettingApprovalMode)
	if err != nil {
		t.Fatal(err)
	}
	return mode
}

// The reviewer's third exploit: a sandboxed curl posted `/mode full-auto` and
// `/approve` as platform cli with no token and changed the person's state.
func TestCLIControlsRequireTheVerifiedLocalPeer(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	t.Setenv("SELF_GATEWAY_TOKEN", "remote-integrator-token")

	attempts := []struct {
		name    string
		peer    LocalPeer
		body    string
		headers []string
	}{
		{"tcp, platform cli", tcpPeer, `{"platform":"cli","platform_user_id":"local","content":"/mode full-auto"}`, nil},
		{"tcp, platform omitted", tcpPeer, `{"content":"/mode full-auto"}`, nil},
		{"tcp, platform spelled CLI", tcpPeer, `{"platform":" CLI ","content":"/mode full-auto"}`, nil},
		{"tcp with the gateway token", tcpPeer, `{"platform":"cli","content":"/mode full-auto"}`,
			[]string{"Authorization", "Bearer remote-integrator-token"}},
		{"tcp with a stolen legacy local-control token", tcpPeer, `{"platform":"cli","content":"/mode full-auto"}`,
			[]string{"X-SelfMind-Local-Control-Token", "stolen", "Authorization", "Bearer remote-integrator-token"}},
		{"socket, refused peer", refusedPeer, `{"platform":"cli","content":"/mode full-auto"}`,
			[]string{"Authorization", "Bearer remote-integrator-token"}},
	}
	for _, attempt := range attempts {
		t.Run(attempt.name, func(t *testing.T) {
			headers := attempt.headers
			if headers == nil {
				headers = []string{"Authorization", "Bearer remote-integrator-token"}
			}
			recorder := serveAs(t, daemon, attempt.peer, http.MethodPost, "/v1/message", attempt.body, headers...)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			if mode := approvalMode(t, store, identity); mode == "full-auto" {
				t.Fatal("an unverified caller changed the person's approval mode")
			}
			approveBody := strings.Replace(attempt.body, "/mode full-auto", "/approve 1", 1)
			recorder = serveAs(t, daemon, attempt.peer, http.MethodPost, "/v1/message", approveBody, headers...)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("/approve status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			current, err := store.GetApprovalRequest(context.Background(), identity.TenantID, approval.ID)
			if err != nil || current.Status != "pending" {
				t.Fatalf("an unverified caller resolved the person's approval: %+v err=%v", current, err)
			}
		})
	}

	// The constraint that must change the result: the person's own CLI.
	auth := []string{"Authorization", "Bearer remote-integrator-token"}
	if recorder := serveAs(t, daemon, verifiedPeer, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/mode full-auto"}`, auth...); recorder.Code != http.StatusOK {
		t.Fatalf("verified /mode status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if mode := approvalMode(t, store, identity); mode != "full-auto" {
		t.Fatalf("the verified CLI could not change its own mode: %q", mode)
	}
	if recorder := serveAs(t, daemon, verifiedPeer, http.MethodPost, "/v1/message", `{"platform":"cli","content":"/approve 1"}`, auth...); recorder.Code != http.StatusOK {
		t.Fatalf("verified /approve status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if current, _ := store.GetApprovalRequest(context.Background(), identity.TenantID, approval.ID); current == nil || current.Status != "approved" {
		t.Fatalf("the verified CLI could not approve: %+v", current)
	}
}

// Every endpoint that resolves or acts with CLI identity applies the same
// rule; local-control endpoints additionally stay closed to every other
// platform.
func TestCLIIdentityEndpointsRefuseUnverifiedPeers(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	cases := []struct {
		method, target, body string
	}{
		{http.MethodPost, "/v1/dispatch", `{"platform":"cli","tool":"skills_list"}`},
		{http.MethodPost, "/v1/approvals/respond", `{"platform":"cli","approval_id":"1","decision":"approve"}`},
		{http.MethodPost, "/v1/runs/steer", `{"platform":"cli","run_id":"run_x","channel":"cli","text":"go"}`},
		{http.MethodGet, "/v1/approvals?platform=cli", ""},
		{http.MethodGet, "/v1/approvals", ""},
		{http.MethodGet, "/v1/tasks", ""},
		{http.MethodGet, "/v1/tasks/current?platform=cli&platform_user_id=local", ""},
		{http.MethodGet, "/v1/sessions?platform=cli", ""},
		{http.MethodGet, "/v1/digest?platform=cli", ""},
		{http.MethodGet, "/v1/presence/ping?platform=cli", ""},
		{http.MethodGet, "/v1/workspaces", ""},
		{http.MethodGet, "/v1/events/stream?platform=cli", ""},
		{http.MethodPost, "/v1/workspaces/register", `{"platform":"cli","local_path":"/tmp/x","name":"x"}`},
		{http.MethodPost, "/v1/workspaces/trust", `{"platform":"cli","trust_level":"trusted"}`},
		{http.MethodGet, "/v1/workspaces/capabilities?platform=cli", ""},
		{http.MethodDelete, "/v1/workspaces/capabilities", `{"platform":"cli","capability":"network:shared"}`},
		{http.MethodPost, "/v1/workspaces/observation-profiles", `{"platform":"cli","script_path":"x.sh"}`},
		{http.MethodPost, "/v1/workspaces/effect-profiles", `{"platform":"cli","script_path":"x.sh"}`},
		{http.MethodPost, "/v1/gateway/model/change", `{"action":"status"}`},
		{http.MethodPost, "/v1/gateway/model/probe", `{"role":"primary"}`},
		{http.MethodPost, "/v1/gateway/tool-catalog/probe", `{}`},
		{http.MethodPost, "/v1/accounts/bind", `{"person_id":"person_x","platform":"telegram","platform_user_id":"attacker"}`},
	}
	for _, tc := range cases {
		for _, peer := range []LocalPeer{tcpPeer, refusedPeer} {
			name := tc.method + " " + tc.target + " socket=" + map[bool]string{true: "refused", false: "tcp"}[peer.Socket]
			t.Run(name, func(t *testing.T) {
				recorder := serveAs(t, daemon, peer, tc.method, tc.target, tc.body,
					"X-SelfMind-Local-Control-Token", "stolen")
				if recorder.Code != http.StatusForbidden {
					t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
				}
			})
		}
	}
	// A verified peer passes the identity gate on the non-streaming endpoints.
	for _, tc := range cases {
		if strings.Contains(tc.target, "/stream") {
			continue
		}
		t.Run("verified "+tc.method+" "+tc.target, func(t *testing.T) {
			recorder := serveAs(t, daemon, verifiedPeer, tc.method, tc.target, tc.body)
			if recorder.Code == http.StatusForbidden && strings.Contains(recorder.Body.String(), "local control socket") {
				t.Fatalf("the verified CLI was refused: %s", recorder.Body.String())
			}
		})
	}
}

// Non-CLI integrations keep their existing authentication over TCP.
func TestNonCLIPlatformsStillWorkOverTCP(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	recorder := serveAs(t, daemon, tcpPeer, http.MethodPost, "/v1/message", `{"platform":"http","platform_user_id":"integrator","content":"/id"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "integrator") {
		t.Fatalf("an HTTP integration identity must keep working over TCP: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = serveAs(t, daemon, tcpPeer, http.MethodGet, "/v1/tasks?platform=telegram&platform_user_id=42", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("an IM identity read must keep working over TCP: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := serveAs(t, daemon, tcpPeer, http.MethodGet, "/health", ""); recorder.Code != http.StatusOK {
		t.Fatalf("health must stay open: %d", recorder.Code)
	}
}

func TestIMIngressCannotClaimTheCLIPlatform(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	for _, target := range []string{"/v1/im/webhook", "/v1/im/cli"} {
		recorder := serveAs(t, daemon, tcpPeer, http.MethodPost, target,
			`{"platform":"cli","platform_user_id":"local","message_id":"m-`+filepath.Base(target)+`","content":"/mode full-auto"}`)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d body=%s", target, recorder.Code, recorder.Body.String())
		}
		if mode := approvalMode(t, store, identity); mode == "full-auto" {
			t.Fatalf("IM ingress at %s changed the CLI person's mode", target)
		}
	}
}

// A browser on any web page can reach a loopback listener. No browser client
// exists, so requests a browser marks as its own are refused, and a state
// change must declare a JSON body, which a cross-origin page cannot send
// without a CORS preflight the daemon never answers.
func TestBrowserOriginatedRequestsAreRejected(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	body := `{"platform":"http","platform_user_id":"page","content":"/id"}`
	for _, headers := range [][]string{
		{"Origin", "http://evil.example"},
		{"Origin", "null"},
		{"Origin", "http://127.0.0.1:8765"},
		{"Sec-Fetch-Site", "cross-site"},
		// A page the browser lets resolve to this address is same-origin to it
		// (DNS rebinding) and sends no Origin on a GET; the Fetch metadata names it.
		{"Sec-Fetch-Site", "same-origin"},
		{"Sec-Fetch-Site", "none"},
	} {
		recorder := serveAs(t, daemon, verifiedPeer, http.MethodPost, "/v1/message", body, headers...)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%v status = %d", headers, recorder.Code)
		}
	}
	// Not a browser: Node's fetch sends `Sec-Fetch-Mode: cors` on every request,
	// but a browser always adds Sec-Fetch-Site, which this client never sends.
	// An HTTP integration written with fetch keeps working.
	if recorder := serveAs(t, daemon, tcpPeer, http.MethodPost, "/v1/message", body, "Sec-Fetch-Mode", "cors", "Accept-Language", "*"); recorder.Code != http.StatusOK {
		t.Fatalf("a Node fetch-style integration was mistaken for a browser: status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	// A GET that a page can trigger with an <img> tag is refused too.
	if recorder := serveAs(t, daemon, tcpPeer, http.MethodGet, "/v1/presence/ping?platform=telegram&platform_user_id=1", "", "Sec-Fetch-Site", "cross-site"); recorder.Code != http.StatusForbidden {
		t.Fatalf("browser GET status = %d", recorder.Code)
	}
	// A form or text/plain post has no preflight; it is not a JSON request.
	for _, contentType := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		request := httptest.NewRequest(http.MethodPost, "/v1/message", strings.NewReader(`{"platform":"cli","content":"/mode full-auto"}`))
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		request = asLocalCLI(request)
		recorder := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content type %q status = %d", contentType, recorder.Code)
		}
	}
	if mode := approvalMode(t, store, identity); mode == "full-auto" {
		t.Fatal("a non-JSON post changed the person's mode")
	}
	// The same JSON post without browser markers is accepted, and the health
	// probe answers whatever asks.
	if recorder := serveAs(t, daemon, verifiedPeer, http.MethodPost, "/v1/message", body, "Content-Type", "application/json; charset=utf-8"); recorder.Code != http.StatusOK {
		t.Fatalf("plain JSON status = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := serveAs(t, daemon, tcpPeer, http.MethodGet, "/health", "", "Origin", "http://evil.example"); recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d", recorder.Code)
	}
}

// Identity is resolved for an HTTP caller only through resolveRequestIdentity,
// which applies the CLI rule. A new handler calling the store directly would
// silently reopen the hole.
func TestHTTPHandlersResolveIdentityOnlyThroughThePeerRule(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	direct := regexp.MustCompile(`ResolveOrCreateAccount\(\s*r\.Context\(\)`)
	for _, file := range files {
		// local_peer.go holds resolveRequestIdentity, the one allowed caller.
		if strings.HasSuffix(file, "_test.go") || file == "local_peer.go" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if direct.Match(data) {
			t.Errorf("%s resolves a request identity without resolveRequestIdentity", file)
		}
	}
}

// /v1/gateway/status tells a caller how the daemon judged that very
// connection, so `selfmind doctor` can say whether this terminal is accepted as
// the person's CLI. It carries no secret and describes only the caller.
func TestGatewayStatusReportsTheCallersOwnVerdict(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	read := func(peer LocalPeer) api.LocalControlPeer {
		t.Helper()
		recorder := serveAs(t, daemon, peer, http.MethodGet, "/v1/gateway/status", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		var status api.GatewayStatusResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status.LocalControl
	}
	if got := read(tcpPeer); got.Socket || got.Verified || got.PID != 0 {
		t.Fatalf("a TCP caller must not appear as a socket peer: %+v", got)
	}
	if got := read(verifiedPeer); !got.Socket || !got.Verified || got.PID != verifiedPeer.PID {
		t.Fatalf("the verified person = %+v", got)
	}
	got := read(refusedPeer)
	if !got.Socket || got.Verified || got.Refusal != refusedPeer.Refusal {
		t.Fatalf("a refused peer must see why: %+v", got)
	}
}

// A connection that never asks for CLI identity must never be judged: the
// verdict reads the peer's credentials, which exist only while the peer is
// connected, and judging a probe that connected and left logs a refusal for a
// peer that asked for nothing. The verdict is taken once, at the first request
// that needs it, while that client is still waiting for its answer.
func TestLocalPeerVerdictIsTakenOnlyWhenARequestNeedsIt(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	judged := 0
	resolve := func() LocalPeer {
		judged++
		return verifiedPeer
	}
	serve := func(ctx context.Context, body string) int {
		request := httptest.NewRequest(http.MethodPost, "/v1/message", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(WithLazyLocalPeer(ctx, resolve))
		recorder := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	connection := context.Background()
	// Ordinary integration traffic and lifecycle probes never ask.
	if code := serve(connection, `{"platform":"http","platform_user_id":"integrator","content":"/id"}`); code != http.StatusOK || judged != 0 {
		t.Fatalf("an HTTP identity must not trigger a judgement: code=%d judged=%d", code, judged)
	}
	// The first CLI claim judges the peer; later ones on the same connection reuse it.
	shared := WithLazyLocalPeer(context.Background(), resolve)
	for i := 0; i < 3; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/message", strings.NewReader(`{"platform":"cli","content":"/id"}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(recorder, request.WithContext(shared))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status %d %s", i, recorder.Code, recorder.Body.String())
		}
	}
	if judged != 1 {
		t.Fatalf("the verdict must be taken exactly once per connection, got %d", judged)
	}
}
