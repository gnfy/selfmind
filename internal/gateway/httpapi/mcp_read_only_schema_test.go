package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

// A control database an earlier development build stamped v26 has no marking
// table. The person gets an explanation they can act on instead of an HTTP
// 500 naming a SQL table, and schema health names the gap.
func TestToolsReadOnlyExplainsAnIncompleteV26Database(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	dir := t.TempDir()
	controltest.SeedDir(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE mcp_read_only_marks`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fixture := &mcpServerFixture{}
	fixture.set(githubServer("mcpsrv_github_a"))
	server := &Server{Control: store, DefaultTenantID: "default", MCPServersFunc: fixture.list}

	for _, content := range []string{"/tools readonly", "/tools readonly github/get_issue on", "/tools readonly github/get_issue off"} {
		response, code := server.ProcessMessage(context.Background(), api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "session-1", Content: content})
		if code != http.StatusOK || !strings.Contains(response.Content, "mcp_read_only_marks") ||
			!strings.Contains(response.Content, "selfmind maintenance restore-control") || strings.Contains(response.Content, "no such table") {
			t.Fatalf("%q: code=%d content=%q error=%q", content, code, response.Content, response.Error)
		}
	}
	diag, _ := server.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "session-1", Content: "/diag tools"})
	if !strings.Contains(diag.Content, "MCP read-only markings are unavailable") {
		t.Fatalf("/diag tools hides the incomplete schema:\n%s", diag.Content)
	}

	request := asLocalCLI(httptest.NewRequest(http.MethodGet, "/v1/gateway/status", nil))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	var status api.GatewayStatusResponse
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status.StoreSchema.MissingTables) != 1 || status.StoreSchema.MissingTables[0] != "mcp_read_only_marks" {
		t.Fatalf("gateway status schema = %+v, want the missing table", status.StoreSchema)
	}
}
