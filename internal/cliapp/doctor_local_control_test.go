package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	gatewayrt "selfmind/internal/runtime/gateway"
)

// doctorChannelFixture stands a fake daemon up on TCP (for the lifecycle
// probes) and, when withSocket is set, on the local control socket, with the
// verdict the daemon would give this process.
type doctorChannelFixture struct {
	app     *App
	socket  string
	tcpURL  string
	closeTC func()
}

func newDoctorChannelFixture(t *testing.T, tcpAnswers, withSocket bool, peer api.LocalControlPeer) *doctorChannelFixture {
	t.Helper()
	t.Setenv("SELF_STORAGE_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	app := &App{ctx: context.Background(), stdout: &strings.Builder{}, stderr: &strings.Builder{}, gatewayEnsured: true}
	socket := gatewayrt.LocalControlSocketPath(app.gatewayDataDir())

	status := func(onSocket bool) api.GatewayStatusResponse {
		response := api.GatewayStatusResponse{State: "running"}
		response.Runtime.State = "running"
		response.Runtime.LocalControlSocket = socket
		if onSocket {
			response.LocalControl = peer
			response.LocalControl.Socket = true
		}
		return response
	}
	serve := func(onSocket bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/gateway/status" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(status(onSocket))
		})
	}
	fixture := &doctorChannelFixture{app: app, socket: socket, closeTC: func() {}}
	if tcpAnswers {
		server := httptest.NewServer(serve(false))
		t.Cleanup(server.Close)
		t.Setenv("SELF_GATEWAY_URL", server.URL)
		fixture.tcpURL = server.URL
	} else {
		// A port nothing listens on: the daemon is not running.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		url := "http://" + listener.Addr().String()
		_ = listener.Close()
		t.Setenv("SELF_GATEWAY_URL", url)
		fixture.tcpURL = url
	}
	if withSocket {
		if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(socket), 0o700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: serve(true)}
		go server.Serve(listener)
		t.Cleanup(func() { _ = server.Close() })
	}
	return fixture
}

func TestDoctorReportsAVerifiedLocalControlChannelAsHealthy(t *testing.T) {
	fixture := newDoctorChannelFixture(t, true, true, api.LocalControlPeer{Verified: true, PID: 4242})
	section := fixture.app.localControlDoctorSection(context.Background())
	for _, want := range []string{"== Local control ==", "status: healthy", fixture.socket, "verified", "4242", "0700"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	if issues := collectDoctorIssues(section, configDiagnostics{}); len(issues) != 0 {
		t.Fatalf("a healthy channel must not be an issue: %+v", issues)
	}
}

// Running the diagnostic from inside a tool the daemon started is the exact
// case the channel refuses; the person must be told how to get out of it.
func TestDoctorReportsARefusedPeerWithItsRemedy(t *testing.T) {
	fixture := newDoctorChannelFixture(t, true, true, api.LocalControlPeer{Refusal: "peer is the daemon or a process it started", PID: 77})
	section := fixture.app.localControlDoctorSection(context.Background())
	for _, want := range []string{"status: refused", "peer is the daemon or a process it started", "your own terminal"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	issues := collectDoctorIssues(section, configDiagnostics{})
	if len(issues) != 1 || issues[0].Category != "CONTROL" || !strings.Contains(issues[0].Details, "refused") {
		t.Fatalf("a refused peer must surface one CONTROL issue: %+v", issues)
	}
}

// A daemon from before the socket answers health on TCP but cannot serve the
// person's CLI; it is named, with the restart that fixes it.
func TestDoctorNamesALegacyDaemonWithoutTheLocalControlSocket(t *testing.T) {
	fixture := newDoctorChannelFixture(t, true, false, api.LocalControlPeer{})
	if err := os.MkdirAll(filepath.Join(fixture.app.gatewayDataDir(), "gateway"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.app.gatewayDataDir(), "gateway", "local_control.token"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	section := fixture.app.localControlDoctorSection(context.Background())
	for _, want := range []string{"status: unreachable", "predates", "legacy token file: present"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	issues := collectDoctorIssues(section, configDiagnostics{})
	if len(issues) != 1 || issues[0].Category != "CONTROL" || len(issues[0].Actions) == 0 ||
		!strings.Contains(strings.Join(issues[0].Actions[0].Commands, " "), "selfmind gateway restart") {
		t.Fatalf("a legacy daemon must offer the restart: %+v", issues)
	}
}

func TestDoctorDoesNotFlagTheChannelWhenTheDaemonIsNotRunning(t *testing.T) {
	fixture := newDoctorChannelFixture(t, false, false, api.LocalControlPeer{})
	section := fixture.app.localControlDoctorSection(context.Background())
	if !strings.Contains(section, "status: not running") {
		t.Fatalf("section = %s", section)
	}
	// The Gateway section already reports a stopped daemon; the channel must
	// not add a second, misleading issue for it.
	if issues := collectDoctorIssues(section, configDiagnostics{}); len(issues) != 0 {
		t.Fatalf("a stopped daemon is not a channel issue: %+v", issues)
	}
}

func TestDoctorFlagsASocketDirectoryOtherUsersCanEnter(t *testing.T) {
	fixture := newDoctorChannelFixture(t, true, true, api.LocalControlPeer{Verified: true, PID: 1})
	if err := os.Chmod(filepath.Dir(fixture.socket), 0o755); err != nil {
		t.Fatal(err)
	}
	section := fixture.app.localControlDoctorSection(context.Background())
	for _, want := range []string{"status: unsafe", "accessible to other users"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	if issues := collectDoctorIssues(section, configDiagnostics{}); len(issues) != 1 || issues[0].Category != "CONTROL" {
		t.Fatalf("an open socket directory must be one CONTROL issue: %+v", issues)
	}
}

// The channel is the person's own CLI to the daemon beside them; a remote
// gateway is reached over TCP and never carries CLI identity.
func TestDoctorSaysARemoteGatewayCannotCarryCLIIdentity(t *testing.T) {
	t.Setenv("SELF_STORAGE_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("SELF_GATEWAY_URL", "http://203.0.113.9:8765")
	app := &App{ctx: context.Background(), stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	section := app.localControlDoctorSection(context.Background())
	for _, want := range []string{"status: remote", "never carry CLI identity"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	_ = errors.New
}

// Exit code 1 means the CLI cannot act as the person; a stopped daemon, a
// remote gateway the person chose, and a healthy channel do not.
func TestDoctorTreatsOnlyABrokenChannelAsFatal(t *testing.T) {
	for status, fatal := range map[string]bool{
		localControlHealthy: false, localControlNotRunning: false, localControlRemote: false,
		localControlRefused: true, localControlUnreachable: true, localControlUnsafe: true,
	} {
		report := "SelfMind doctor\n\n== Local control ==\nsocket: /x\nstatus: " + status + "\n\n== Config ==\nstatus: ok"
		if got := doctorReportHasFatalIssue(report); got != fatal {
			t.Fatalf("status %q: fatal = %v, want %v", status, got, fatal)
		}
	}
}

// The bundle carries the section between the gateway and the configuration.
func TestDoctorBundlePlacesLocalControlAfterTheGateway(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "doctor-channel", "Doctor")
	if err != nil {
		t.Fatal(err)
	}
	report := buildDoctorReport(ctx, store, identity, dataDir, "not running",
		"== Local control ==\nsocket: /x\nstatus: healthy", "", "", 5)
	gateway := strings.Index(report, "== Gateway ==")
	channel := strings.Index(report, "== Local control ==")
	if gateway < 0 || channel < gateway {
		t.Fatalf("the channel must follow the gateway section:\n%s", report)
	}
}
