package cliapp

import (
	"bytes"
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
