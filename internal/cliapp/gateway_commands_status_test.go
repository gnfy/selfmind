package cliapp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		},
	})
	for _, want := range []string{"mcp servers: 1 connected / 2 configured, 1 failed", "github: connection refused"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("gateway status missing %q:\n%s", want, out.String())
		}
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
