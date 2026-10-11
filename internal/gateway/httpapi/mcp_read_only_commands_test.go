package httpapi

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// mcpServerFixture is a mutable view of the configured MCP servers, so a test
// can restart the daemon with another configuration.
type mcpServerFixture struct {
	mu      sync.Mutex
	servers []tools.MCPServerStatus
}

func (f *mcpServerFixture) set(servers ...tools.MCPServerStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = servers
}

func (f *mcpServerFixture) list() []tools.MCPServerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tools.MCPServerStatus(nil), f.servers...)
}

func githubServer(identity string) tools.MCPServerStatus {
	return tools.MCPServerStatus{Name: "github", Identity: identity, Connected: true,
		Tools: []string{"create_issue", "get_issue", "list_issues"}, OperatorReadOnly: []string{"list_issues", "search_code"}}
}

func newToolsCommandServer(t *testing.T) (*Server, *control.Store, *mcpServerFixture) {
	t.Helper()
	server, store, fixture, _ := newToolsCommandServerInDir(t)
	return server, store, fixture
}

func newToolsCommandServerInDir(t *testing.T) (*Server, *control.Store, *mcpServerFixture, string) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	fixture := &mcpServerFixture{}
	fixture.set(githubServer("mcpsrv_github_a"),
		tools.MCPServerStatus{Name: "notion", Identity: "mcpsrv_notion", Connected: false})
	return &Server{Control: store, DefaultTenantID: "default", MCPServersFunc: fixture.list}, store, fixture, dir
}

func sendCommand(t *testing.T, server *Server, request api.MessageRequest) string {
	t.Helper()
	response, code := server.ProcessMessage(context.Background(), request)
	if code != http.StatusOK {
		t.Fatalf("%q: status %d: %+v", request.Content, code, response)
	}
	return response.Content
}

// A person marks one exact tool from one endpoint, sees it listed with its
// audit, and withdraws it from another of their endpoints. Another person
// neither sees nor changes it.
func TestToolsReadOnlyCommandMarksListsAndUnmarksForThePerson(t *testing.T) {
	server, store, _, dir := newToolsCommandServerInDir(t)
	ctx := context.Background()
	cli := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-7"}
	command := func(base api.MessageRequest, content string) string {
		base.Content = content
		return sendCommand(t, server, base)
	}

	reply := command(cli, "/tools readonly github/get_issue on")
	if !strings.Contains(reply, "Marked github/get_issue read-only for you") || !strings.Contains(reply, "/tools readonly github/get_issue off") {
		t.Fatalf("mark reply = %q", reply)
	}
	owner, err := store.ResolveAccount(ctx, "default", "cli", "local")
	if err != nil || owner == nil {
		t.Fatalf("owner account: %+v %v", owner, err)
	}
	marks, err := store.ListMCPReadOnlyMarks(ctx, owner.TenantID, owner.PersonID)
	if err != nil || len(marks) != 1 || marks[0].ServerIdentity != "mcpsrv_github_a" || marks[0].ToolName != "get_issue" ||
		marks[0].MarkedByAccountID != owner.AccountID || marks[0].MarkedChannel != "session-7" {
		t.Fatalf("recorded marking = %+v err=%v", marks, err)
	}
	if again := command(cli, "/tools readonly github/get_issue on"); !strings.Contains(again, "already marked read-only") {
		t.Fatalf("second mark = %q", again)
	}

	listing := command(cli, "/tools readonly")
	for _, want := range []string{"github (connected)", "get_issue: you, marked", "from session-7",
		"list_issues: operator configuration", "search_code: operator configuration (the server does not expose it",
		"notion (not connected)", "/tools readonly <server>/<tool> on"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing lacks %q:\n%s", want, listing)
		}
	}
	if bare := command(cli, "/tools"); bare != listing {
		t.Fatalf("bare /tools differs from the listing:\n%s\n---\n%s", bare, listing)
	}

	stranger := api.MessageRequest{Platform: "telegram", PlatformUserID: "900", Channel: "900"}
	if strangerList := command(stranger, "/tools readonly"); strings.Contains(strangerList, "get_issue: you") {
		t.Fatalf("another person sees the marking:\n%s", strangerList)
	}
	if strangerOff := command(stranger, "/tools readonly github/get_issue off"); !strings.Contains(strangerOff, "You have not marked github/get_issue") {
		t.Fatalf("another person's unmark = %q", strangerOff)
	}
	if marked, _ := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, owner.PersonID, "github", "mcpsrv_github_a", "get_issue", time.Time{}); !marked {
		t.Fatal("another person withdrew the marking")
	}

	phone, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "777", "Owner phone")
	if err != nil {
		t.Fatal(err)
	}
	im := api.MessageRequest{Platform: "telegram", PlatformUserID: "777", Channel: "777"}
	if off := command(im, "/tools readonly github/get_issue off"); !strings.Contains(off, "Unmarked github/get_issue") {
		t.Fatalf("unmark from IM = %q", off)
	}
	if marked, _ := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, owner.PersonID, "github", "mcpsrv_github_a", "get_issue", time.Time{}); marked {
		t.Fatal("the unmarked tool is still marked")
	}
	audit, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	var revokedBy, revokedChannel string
	if err := audit.QueryRow(`SELECT revoked_by_account_id, revoked_channel FROM mcp_read_only_marks
		WHERE person_id = ? AND tool_name = 'get_issue'`, owner.PersonID).Scan(&revokedBy, &revokedChannel); err != nil ||
		revokedBy != phone.AccountID || revokedChannel != "777" {
		t.Fatalf("revocation audit = %q %q err=%v", revokedBy, revokedChannel, err)
	}
	if off := command(cli, "/tools readonly github/list_issues off"); !strings.Contains(off, "operator configuration marks it read-only") {
		t.Fatalf("unmarking an operator marking = %q", off)
	}
}

// Names are exact and must name a tool the connected server exposes now.
func TestToolsReadOnlyCommandRefusesInexactOrUnverifiableTools(t *testing.T) {
	server, store, _ := newToolsCommandServer(t)
	cli := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-1"}
	for content, want := range map[string]string{
		"/tools readonly github/get_* on":        "does not expose a tool named get_*",
		"/tools readonly github/GET_ISSUE on":    "does not expose a tool named GET_ISSUE",
		"/tools readonly gitlab/get_issue on":    "No configured MCP server",
		"/tools readonly notion/search on":       "MCP server notion is not connected",
		"/tools readonly github/get_issue maybe": "Usage: /tools readonly",
		"/tools writable github/get_issue on":    "Usage: /tools readonly",
	} {
		cli.Content = content
		if reply := sendCommand(t, server, cli); !strings.Contains(reply, want) {
			t.Errorf("%q = %q, want %q", content, reply, want)
		}
	}
	owner, err := store.ResolveAccount(context.Background(), "default", "cli", "local")
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	if marks, err := store.ListMCPReadOnlyMarks(context.Background(), owner.TenantID, owner.PersonID); err != nil || len(marks) != 0 {
		t.Fatalf("a refused command recorded %+v err=%v", marks, err)
	}
}

// A server configured anew under the same name is another server: the old
// marking no longer applies, and the person's listing and diagnostics say so.
func TestToolsReadOnlyListingShowsMarkingsDroppedByAServerChange(t *testing.T) {
	server, _, fixture := newToolsCommandServer(t)
	cli := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-1"}
	cli.Content = "/tools readonly github/get_issue on"
	sendCommand(t, server, cli)

	fixture.set(githubServer("mcpsrv_github_b"))
	cli.Content = "/tools readonly"
	listing := sendCommand(t, server, cli)
	if !strings.Contains(listing, "get_issue: you") || !strings.Contains(listing, "inactive: the server's configuration changed since you marked it") {
		t.Fatalf("changed identity did not show the dropped marking:\n%s", listing)
	}
	cli.Content = "/diag tools"
	if diag := sendCommand(t, server, cli); !strings.Contains(diag, "1 of your MCP read-only markings no longer applies") {
		t.Fatalf("diagnostics hide the dropped marking:\n%s", diag)
	}

	// Marking again binds the new identity; the old marking stays inactive.
	cli.Content = "/tools readonly github/get_issue on"
	if reply := sendCommand(t, server, cli); !strings.Contains(reply, "Marked github/get_issue") {
		t.Fatalf("re-mark = %q", reply)
	}
	cli.Content = "/diag tools"
	if diag := sendCommand(t, server, cli); !strings.Contains(diag, "1 of your MCP read-only markings no longer applies") {
		t.Fatalf("re-marking hid the old marking:\n%s", diag)
	}

	// A server removed from the configuration leaves the marking listed and
	// revocable.
	fixture.set()
	cli.Content = "/tools readonly"
	listing = sendCommand(t, server, cli)
	if !strings.Contains(listing, "inactive: no MCP server named github is configured") {
		t.Fatalf("removed server:\n%s", listing)
	}
	cli.Content = "/tools readonly github/get_issue off"
	if reply := sendCommand(t, server, cli); !strings.Contains(reply, "Unmarked github/get_issue") {
		t.Fatalf("unmark for a removed server = %q", reply)
	}
	cli.Content = "/tools readonly"
	if listing := sendCommand(t, server, cli); !strings.Contains(listing, "No MCP servers are configured") {
		t.Fatalf("empty listing = %q", listing)
	}
}

// A server name may contain "/". When a reference splits into more than one
// configured server and tool, SelfMind names the readings instead of picking
// one; a reference with one reading still works.
func TestToolsReadOnlyRefusesAnAmbiguousServerSplit(t *testing.T) {
	server, store, fixture := newToolsCommandServer(t)
	fixture.set(
		tools.MCPServerStatus{Name: "gh", Identity: "mcpsrv_gh", Connected: true, Tools: []string{"enterprise/get_issue", "get_issue"}},
		tools.MCPServerStatus{Name: "gh/enterprise", Identity: "mcpsrv_ghe", Connected: true, Tools: []string{"get_issue"}},
	)
	cli := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-1"}
	for _, action := range []string{"on", "off"} {
		cli.Content = "/tools readonly gh/enterprise/get_issue " + action
		reply := sendCommand(t, server, cli)
		for _, want := range []string{"ambiguous", "server gh, tool enterprise/get_issue", "server gh/enterprise, tool get_issue"} {
			if !strings.Contains(reply, want) {
				t.Fatalf("%s: reply %q lacks %q", action, reply, want)
			}
		}
	}
	owner, err := store.ResolveAccount(context.Background(), "default", "cli", "local")
	if err != nil || owner == nil {
		t.Fatalf("owner: %+v %v", owner, err)
	}
	if marks, err := store.ListMCPReadOnlyMarks(context.Background(), owner.TenantID, owner.PersonID); err != nil || len(marks) != 0 {
		t.Fatalf("an ambiguous reference recorded %+v err=%v", marks, err)
	}
	cli.Content = "/tools readonly gh/get_issue on"
	if reply := sendCommand(t, server, cli); !strings.Contains(reply, "Marked gh/get_issue read-only for you") {
		t.Fatalf("unambiguous reference = %q", reply)
	}
	if marks, err := store.ListMCPReadOnlyMarks(context.Background(), owner.TenantID, owner.PersonID); err != nil ||
		len(marks) != 1 || marks[0].ServerName != "gh" || marks[0].ToolName != "get_issue" {
		t.Fatalf("unambiguous marking = %+v err=%v", marks, err)
	}
}
