package cliapp

import (
	"path/filepath"
	"strings"
	"testing"

	gatewayrt "selfmind/internal/runtime/gateway"
)

// A daemon that binds every interface (gateway.addr ":8765", "0.0.0.0:8765",
// "[::]:8765") is still this machine's daemon. The CLI used to read its own
// address as a remote gateway: every CLI-identity command and the TUI then went
// over TCP and got a refusal nobody could act on.
func TestTheCLITreatsAWildcardAddressAsItsOwnDaemon(t *testing.T) {
	for addr, local := range map[string]bool{
		"127.0.0.1:8765":    true,
		"localhost:8765":    true,
		"[::1]:8765":        true,
		":8765":             true,
		"0.0.0.0:8765":      true,
		"[::]:8765":         true,
		"203.0.113.9:8765":  false,
		"10.0.0.5:8765":     false,
		"gateway.example:1": false,
	} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("SELF_GATEWAY_URL", "")
			t.Setenv("SELF_DAEMON_URL", "")
			t.Setenv("SELF_GATEWAY_ADDR", addr)
			app := &App{}
			if got := app.gatewayTargetIsLocal(); got != local {
				t.Fatalf("gatewayTargetIsLocal() for %q (%s) = %v, want %v", addr, app.gatewayURL(), got, local)
			}
		})
	}
	// The same answer for an explicit URL, whatever its scheme or port.
	for url, local := range map[string]bool{
		"http://0.0.0.0:8765":       true,
		"http://[::]:8765":          true,
		"http://:8765":              true,
		"https://localhost:8443":    true,
		"https://gateway.example":   false,
		"http://203.0.113.9:8765":   false,
		"http://127.0.0.1.evil.com": false,
	} {
		t.Run(url, func(t *testing.T) {
			t.Setenv("SELF_GATEWAY_URL", url)
			if got := (&App{}).gatewayTargetIsLocal(); got != local {
				t.Fatalf("gatewayTargetIsLocal() for %q = %v, want %v", url, got, local)
			}
		})
	}
}

// The daemon no longer tells an unauthenticated caller where its socket is; the
// CLI knows its own data directory and shows the person the path after a refusal.
func TestARefusalShowsThePersonWhereTheSocketIsWithoutTheDaemonSayingSo(t *testing.T) {
	t.Setenv("SELF_STORAGE_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	app := &App{}
	socket := gatewayrt.LocalControlSocketPath(app.gatewayDataDir())
	refusal := []byte(`{"error":"CLI identity requires the person's own selfmind CLI on the daemon's local control socket; requests over TCP never carry CLI identity."}`)

	t.Setenv("SELF_GATEWAY_URL", "")
	t.Setenv("SELF_GATEWAY_ADDR", "127.0.0.1:8765")
	if got := app.gatewayErrorLine("403 Forbidden", refusal); !strings.Contains(got, socket) || !strings.Contains(got, "requires the person's own selfmind CLI") {
		t.Fatalf("a refusal on a local daemon must show the socket path %s: %s", socket, got)
	}
	// A refusal that does not concern the socket, and other statuses, say what the daemon said.
	for status, body := range map[string]string{
		"403 Forbidden":             `{"error":"workspace is untrusted"}`,
		"500 Internal Server Error": `{"error":"the daemon's local control socket failed"}`,
	} {
		if got := app.gatewayErrorLine(status, []byte(body)); strings.Contains(got, socket) {
			t.Fatalf("%s %s must not be annotated: %s", status, body, got)
		}
	}
	// A remote gateway has no socket on this machine to point at.
	t.Setenv("SELF_GATEWAY_URL", "https://gateway.example")
	if got := app.gatewayErrorLine("403 Forbidden", refusal); strings.Contains(got, socket) {
		t.Fatalf("a remote gateway's refusal must not name a local path: %s", got)
	}
}

// The runtime and the CLI decide "is this daemon on this host" with one
// predicate: where they disagreed, the CLI chose the socket and the runtime TCP.
func TestTheCLIAndTheRuntimeAgreeOnWhichDaemonIsLocal(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:8765", "http://localhost:8765", "http://[::1]:8765", "http://:8765",
		"http://0.0.0.0:8765", "http://[::]:8765", "http://203.0.113.9:8765", "https://gateway.example",
	} {
		t.Setenv("SELF_GATEWAY_URL", url)
		if cli, runtime := (&App{}).gatewayTargetIsLocal(), gatewayrt.LocalGatewayURL(url); cli != runtime {
			t.Fatalf("%s: the CLI says local=%v, the runtime says local=%v", url, cli, runtime)
		}
	}
}
