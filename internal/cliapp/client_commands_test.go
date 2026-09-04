package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestSendCarriesInvocationAdditionalRoots(t *testing.T) {
	root := t.TempDir()
	app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "send", "inspect both roots"})
	app.additionalDirs = []string{root}

	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if len(recorded.ClientAdditionalRoots) != 1 || recorded.ClientAdditionalRoots[0] != root {
		t.Fatalf("client_additional_roots = %#v", recorded.ClientAdditionalRoots)
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

// TestSearchCommandUsesShortLivedGatewayRequest: history search is a
// short-lived CLI call now, not a terminal-only affordance, so `selfmind
// search` reaches the same gateway command every endpoint uses.
func TestSearchCommandUsesShortLivedGatewayRequest(t *testing.T) {
	app, recorded, stdout, _ := newSendTestApp(t, []string{"selfmind", "search", "aurora gate"})

	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.Content != "/search aurora gate" {
		t.Fatalf("content = %q", recorded.Content)
	}
	if strings.TrimSpace(stdout.String()) != "ok" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUsageCommandUsesDailyExecutionAndCostReport(t *testing.T) {
	app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "usage"})
	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.Content != "/report daily --since 24h" {
		t.Fatalf("content = %q", recorded.Content)
	}
}

func TestStatusShowsPersonalPromptDiscoverabilityHint(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, recorded, stdout, _ := newSendTestApp(t, []string{"selfmind", "status"})
	app.configPath = configPath
	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d", handled, code)
	}
	if recorded.Content != "/status" {
		t.Fatalf("content=%q", recorded.Content)
	}
	if !strings.Contains(stdout.String(), "selfmind prompt edit agent") || !strings.Contains(stdout.String(), "agent.md") {
		t.Fatalf("status omitted personal prompt hint: %s", stdout.String())
	}
}

func TestReportDailyUsesShortLivedGatewayRequest(t *testing.T) {
	app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "report", "daily", "--since", "48h"})
	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.Content != "/report daily --since 48h" {
		t.Fatalf("content = %q", recorded.Content)
	}
}

func TestWatchersUsesShortLivedGatewayRequest(t *testing.T) {
	app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "watchers", "attention"})
	handled, code := app.runGatewayClientIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.Content != "/watchers attention" {
		t.Fatalf("content = %q", recorded.Content)
	}
}

func TestExtractTaskResumeCommand(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{args: []string{"selfmind", "resume", "task_12345678"}, stdout: stdout, stderr: stderr}

	handled, code := app.extractTaskResumeCommand()
	if !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if app.resumeTaskRef != "task_12345678" {
		t.Fatalf("resumeTaskRef = %q", app.resumeTaskRef)
	}
	if len(app.args) != 1 || app.args[0] != "selfmind" {
		t.Fatalf("args after extraction = %+v", app.args)
	}
}

// TestBareResumeIsTheAttentionListing: a bare `selfmind resume` is not a usage
// error. It is the listing of what needs the person — the surface that
// replaced `selfmind tasks` — so the interactive-pin path must decline it and
// let the short-lived client command answer.
func TestBareResumeIsTheAttentionListing(t *testing.T) {
	stderr := &bytes.Buffer{}
	app := &App{args: []string{"selfmind", "resume"}, stderr: stderr}
	if handled, code := app.extractTaskResumeCommand(); handled || code != 0 {
		t.Fatalf("bare resume must not be claimed by the pin path: handled=%v code=%d", handled, code)
	}
	if strings.TrimSpace(stderr.String()) != "" {
		t.Fatalf("bare resume must not print a usage error: %q", stderr.String())
	}

	app2, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "resume"})
	if handled, code := app2.runGatewayClientIfRequested(); !handled || code != 0 {
		t.Fatalf("handled = %v, code = %d", handled, code)
	}
	if recorded.Content != "/resume" {
		t.Fatalf("content = %q", recorded.Content)
	}

	// A reference still pins the interactive session to one exact run.
	app3 := &App{args: []string{"selfmind", "resume", "run_123"}, stderr: &bytes.Buffer{}}
	if handled, code := app3.extractTaskResumeCommand(); !handled || code != 0 {
		t.Fatalf("a reference must be claimed by the pin path: handled=%v code=%d", handled, code)
	}
	if app3.resumeTaskRef != "run_123" {
		t.Fatalf("resume ref = %q", app3.resumeTaskRef)
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
