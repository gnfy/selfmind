package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

func TestToolsDiagShowsSchemaHealthWithoutRawSchema(t *testing.T) {
	server := &Server{
		ToolSchemaReportFunc: func() []tools.ToolSchemaReport {
			return []tools.ToolSchemaReport{
				{Name: "read_file", Origin: tools.ToolSchemaOriginBuiltin, Status: tools.ToolSchemaActive, Hash: "aaaa"},
				{Name: "mcp_bad", Origin: tools.ToolSchemaOriginExternal, Status: tools.ToolSchemaQuarantined, Hash: "bbbb", Issues: []tools.ToolSchemaIssue{{Severity: tools.ToolSchemaError, Code: "invalid_required", Path: "$.required", Message: "secret schema text"}}},
			}
		},
	}
	handled, reply, err := server.tryHandleControlCommand(context.Background(), &control.IdentityContext{}, api.MessageRequest{Content: "/diag tools"})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	for _, want := range []string{"Tool schema diagnostics", "1 active, 0 repaired, 1 quarantined", "mcp_bad", "invalid_required at $.required"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
	if strings.Contains(reply, "secret schema text") {
		t.Fatalf("diagnostics leaked raw issue message:\n%s", reply)
	}
}
