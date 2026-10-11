package cliapp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestPrintGatewayStatusIncludesBoundedMCPHealth(t *testing.T) {
	out := &bytes.Buffer{}
	app := &App{stdout: out}
	app.printGatewayStatus(api.GatewayStatusResponse{
		State: "running",
		MCP: api.MCPHealth{
			Configured: 2, Connected: 1, Failed: 1,
			Failures: []api.MCPServerFailure{{Name: "github", Error: "connection refused"}},
			Warnings: []api.MCPServerWarning{{Name: "notion", Warning: "read_only_tools names tools this server does not expose: serch"}},
		},
	})
	for _, want := range []string{"mcp servers: 1 connected / 2 configured, 1 failed", "github: connection refused",
		"notion: warning: read_only_tools names tools this server does not expose: serch"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("gateway status missing %q:\n%s", want, out.String())
		}
	}
}

// A daemon answers a caller that is not the person's own CLI with liveness only.
// The status must say so and say what to do, not print a bare "running" that
// looks like a daemon with nothing to report.
func TestGatewayStatusSaysWhyItIsLivenessOnly(t *testing.T) {
	cases := []struct {
		name   string
		status api.GatewayStatusResponse
		want   []string
	}{
		{"a tool the daemon refused on the socket", api.GatewayStatusResponse{
			State: "running", LivenessOnly: true,
			LocalControl: api.LocalControlPeer{Socket: true, Refusal: "peer is a process the daemon started"},
		}, []string{"liveness only", "peer is a process the daemon started", "your own terminal"}},
		{"a request over TCP without the token", api.GatewayStatusResponse{State: "running", LivenessOnly: true},
			[]string{"liveness only", "gateway token", "your own terminal"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			(&App{stdout: out}).printGatewayStatus(tc.status)
			for _, want := range append([]string{"SelfMind gateway: running"}, tc.want...) {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("status missing %q:\n%s", want, out.String())
				}
			}
		})
	}
	// The constraint that must change the result: a full answer carries no hint.
	out := &bytes.Buffer{}
	(&App{stdout: out}).printGatewayStatus(api.GatewayStatusResponse{State: "running", Runtime: api.GatewayRuntimeInfo{PID: 4242}})
	if strings.Contains(out.String(), "liveness only") {
		t.Fatalf("a full status must not claim to be liveness only:\n%s", out.String())
	}
}

// A database stamped with the current version but missing a table is not a
// healthy schema: status and doctor name the table and the way back.
func TestControlSchemaHealthNamesMissingTables(t *testing.T) {
	out := &bytes.Buffer{}
	app := &App{stdout: out}
	app.printGatewayStatus(api.GatewayStatusResponse{State: "running", StoreSchema: api.StoreSchemaHealth{
		Version: 26, CurrentVersion: 26, MissingTables: []string{"mcp_read_only_marks"}}})
	if !strings.Contains(out.String(), "control schema: v26 (binary supports v26), missing tables: mcp_read_only_marks") ||
		!strings.Contains(out.String(), "selfmind maintenance restore-control") {
		t.Fatalf("gateway status hides the missing table:\n%s", out.String())
	}
	line := formatControlSchemaLine(control.StoreSchemaStatus{Version: 26, CurrentVersion: 26, MissingTables: []string{"mcp_read_only_marks"}})
	if line != "control schema: 26/26 (backup: none; missing tables: mcp_read_only_marks)" {
		t.Fatalf("doctor schema line = %q", line)
	}
}

func TestPromptCustomizationHintOnlyWhenAgentFileIsMissing(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	app := &App{stdout: out, configPath: configPath}
	app.printPromptCustomizationHint()
	if !strings.Contains(out.String(), "selfmind prompt edit agent") || !strings.Contains(out.String(), "agent.md") {
		t.Fatalf("missing discoverability hint: %s", out.String())
	}

	promptPath := filepath.Join(filepath.Dir(configPath), "prompts", "agent.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("## Persona\n\ndefault\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	app.printPromptCustomizationHint()
	if out.Len() != 0 {
		t.Fatalf("configured prompt should suppress hint: %s", out.String())
	}
}

// A current daemon reports where the person's own CLI reaches it; a daemon from
// before the socket reports none, which `gateway status` says plainly.
func TestPrintGatewayStatusNamesTheLocalControlSocket(t *testing.T) {
	out := &bytes.Buffer{}
	app := &App{stdout: out}
	status := api.GatewayStatusResponse{State: "running"}
	status.Runtime.Addr = "127.0.0.1:8765"
	status.Runtime.LocalControlSocket = "/home/u/.selfmind/data/gateway/control.sock"
	app.printGatewayStatus(status)
	if !strings.Contains(out.String(), "local control: /home/u/.selfmind/data/gateway/control.sock") {
		t.Fatalf("gateway status hides the socket:\n%s", out.String())
	}

	legacy := &bytes.Buffer{}
	app = &App{stdout: legacy}
	status.Runtime.LocalControlSocket = ""
	app.printGatewayStatus(status)
	if !strings.Contains(legacy.String(), "local control: none") || !strings.Contains(legacy.String(), "selfmind gateway restart") {
		t.Fatalf("a daemon without the socket must say so and name the restart:\n%s", legacy.String())
	}
}
