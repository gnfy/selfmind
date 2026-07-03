package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
)

func TestGatewayURLPrefersGatewayEnv(t *testing.T) {
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:9001/")
	t.Setenv("SELF_DAEMON_URL", "http://127.0.0.1:9002/")
	t.Setenv("SELF_GATEWAY_ADDR", "127.0.0.1:9003")
	t.Setenv("SELF_DAEMON_ADDR", "127.0.0.1:9004")

	app := &App{}
	if got, want := app.gatewayURL(), "http://127.0.0.1:9001"; got != want {
		t.Fatalf("gatewayURL() = %q, want %q", got, want)
	}
}

func TestGatewayURLFallsBackToDaemonEnv(t *testing.T) {
	t.Setenv("SELF_GATEWAY_URL", "")
	t.Setenv("SELF_DAEMON_URL", "")
	t.Setenv("SELF_GATEWAY_ADDR", "")
	t.Setenv("SELF_DAEMON_ADDR", "127.0.0.1:9004")

	app := &App{}
	if got, want := app.gatewayURL(), "http://127.0.0.1:9004"; got != want {
		t.Fatalf("gatewayURL() = %q, want %q", got, want)
	}
}

// newSendTestApp wires an App at a fake gateway that records the decoded
// MessageRequest, with daemon auto-start disabled.
func newSendTestApp(t *testing.T, args []string) (*App, *api.MessageRequest, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	recorded := &api.MessageRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/message" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(recorded)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.MessageResponse{Content: "ok"})
	}))
	t.Cleanup(server.Close)
	t.Setenv("SELF_GATEWAY_URL", server.URL)
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:    context.Background(),
		args:   args,
		stdout: stdout,
		stderr: stderr,
		// Skip local daemon auto-start; the fake gateway is already "running".
		gatewayEnsured: true,
	}
	return app, recorded, stdout, stderr
}

func TestSendModeFlagThreadsApprovalMode(t *testing.T) {
	app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "send", "--mode", "auto-edit", "hello", "world"})

	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.ApprovalMode != "auto-edit" {
		t.Fatalf("approval_mode = %q, want auto-edit", recorded.ApprovalMode)
	}
	if recorded.Content != "hello world" {
		t.Fatalf("content = %q", recorded.Content)
	}
	if recorded.Async {
		t.Fatal("async should be false")
	}
}

func TestSendModeEqualsSyntaxAndAsync(t *testing.T) {
	app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "send", "--async", "--mode=full-auto", "run it"})

	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.ApprovalMode != "full-auto" || !recorded.Async {
		t.Fatalf("approval_mode = %q, async = %v", recorded.ApprovalMode, recorded.Async)
	}
}

func TestSendRejectsInvalidMode(t *testing.T) {
	app, recorded, _, stderr := newSendTestApp(t, []string{"selfmind", "send", "--mode", "yolo", "hello"})

	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 2 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if !strings.Contains(stderr.String(), "invalid --mode") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if recorded.Content != "" {
		t.Fatalf("request should not be sent, got %+v", recorded)
	}
}

func TestGatewayErrorLineExtractsJSONError(t *testing.T) {
	if got := gatewayErrorLine("500 Internal Server Error", []byte(`{"error":"approval request not found: apr_x"}`)); got != "approval request not found: apr_x" {
		t.Fatalf("got %q", got)
	}
	if got := gatewayErrorLine("502 Bad Gateway", []byte("upstream down")); got != "502 Bad Gateway: upstream down" {
		t.Fatalf("got %q", got)
	}
	if got := gatewayErrorLine("503 Service Unavailable", nil); got != "503 Service Unavailable" {
		t.Fatalf("got %q", got)
	}
}

func TestPlatformUserIDPrefersExplicitEnv(t *testing.T) {
	t.Setenv("SELF_PLATFORM_USER_ID", "person-local")
	t.Setenv("USERNAME", "windows-user")
	t.Setenv("USER", "unix-user")

	if got, want := platformUserID(), "person-local"; got != want {
		t.Fatalf("platformUserID() = %q, want %q", got, want)
	}
}
