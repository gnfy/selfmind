package tools

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpStdioHelperArg = "selfmind-mcp-stdio-helper"

type mcpEchoInput struct {
	Text string `json:"text" jsonschema:"text to echo"`
}

func TestMCPClientStdioDiscoversPagedToolsAndCallsTool(t *testing.T) {
	if slices.Contains(os.Args, mcpStdioHelperArg) {
		server := newMCPTestServer(1)
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}

	client, err := NewMCPClient(MCPServerConfig{
		Name:      "stdio-test",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=^TestMCPClientStdioDiscoversPagedToolsAndCallsTool$", "--", mcpStdioHelperArg},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if got := len(client.GetTools()); got != 2 {
		t.Fatalf("discovered %d tools, want 2: %#v", got, client.GetTools())
	}
	result, err := client.CallTool("echo", map[string]interface{}{"text": "connected"})
	if err != nil {
		t.Fatal(err)
	}
	if result != "connected" {
		t.Fatalf("tool result = %q, want connected", result)
	}
}

func TestMCPClientStreamableHTTPForwardsHeadersAndCallsTool(t *testing.T) {
	server := newMCPTestServer(1)
	var sawHeaders atomic.Bool
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-token" && r.Header.Get("X-SelfMind-Test") == "present" {
			sawHeaders.Store(true)
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()

	client, err := NewMCPClient(MCPServerConfig{
		Name:      "http-test",
		Transport: "http",
		URL:       httpServer.URL,
		Headers:   map[string]string{"X-SelfMind-Test": "present"},
		Auth:      map[string]string{"bearer": "test-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	result, err := client.CallTool("echo", map[string]interface{}{"text": "remote"})
	if err != nil {
		t.Fatal(err)
	}
	if result != "remote" {
		t.Fatalf("tool result = %q, want remote", result)
	}
	if !sawHeaders.Load() {
		t.Fatal("configured HTTP headers were not forwarded")
	}
}

func TestMCPClientDoesNotForwardRuntimeArguments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var received map[string]interface{}
	server := mcp.NewServer(&mcp.Implementation{Name: "argument-boundary-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "capture", Description: "capture public arguments"}, func(_ context.Context, _ *mcp.CallToolRequest, input map[string]interface{}) (*mcp.CallToolResult, any, error) {
		received = input
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "captured"}}}, nil, nil
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client, err := newMCPClientWithTransport(ctx, MCPServerConfig{Name: "argument-boundary", Transport: "in-memory"}, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	args := map[string]interface{}{
		"text":              "public",
		"nested":            map[string]interface{}{"_business_key": "preserved"},
		"_tenant_id":        "tenant-secret",
		"_invocation_scope": map[string]interface{}{"person_id": "person-secret", "run_id": "run-secret"},
		"_clarify_fn":       func(string, []string) string { return "yes" },
	}
	if _, err := client.CallTool("capture", args); err != nil {
		t.Fatalf("CallTool failed after runtime filtering: %v", err)
	}
	if len(received) != 2 || received["text"] != "public" {
		t.Fatalf("remote arguments = %#v", received)
	}
	nested, _ := received["nested"].(map[string]interface{})
	if nested["_business_key"] != "preserved" {
		t.Fatalf("nested business argument was stripped: %#v", nested)
	}
	if _, exists := args["_tenant_id"]; !exists {
		t.Fatal("CallTool mutated the caller's argument map")
	}
}

func TestMCPClientRejectsUnavailableHTTPServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := NewMCPClient(MCPServerConfig{
		Name:      "unavailable",
		Transport: "http",
		URL:       "http://" + addr + "/mcp",
	})
	if err == nil {
		_ = client.Close()
		t.Fatal("unavailable MCP server was reported as connected")
	}
}

func TestMCPToolManagerTracksDynamicCatalogueChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := newMCPTestServer(1)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	dispatcher := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(dispatcher)
	config := MCPServerConfig{Name: "dynamic", Transport: "in-memory"}
	client, err := newMCPClientWithTransport(ctx, config, clientTransport, func(changedClient *MCPClient, tools map[string]MCPToolDef) {
		manager.syncTools(config.Name, changedClient, tools)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.clients[config.Name] = client
	manager.registered[config.Name] = make(map[string]struct{})
	manager.syncTools(config.Name, client, client.GetTools())

	lateName := MCPToolLocalName(config.Name, "late")
	addMCPTestTool(server, "late")
	waitForMCPTool(t, dispatcher, lateName, true)

	echoName := MCPToolLocalName(config.Name, "echo")
	server.RemoveTools("echo")
	waitForMCPTool(t, dispatcher, echoName, false)
}

func TestMCPToolManagerRejectsRegistryCollisionAndReportsCurrentHealth(t *testing.T) {
	dispatcher := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(dispatcher)
	manager.configured = 1
	serverName := "external"
	remoteName := "search"
	localName := MCPToolLocalName(serverName, remoteName)

	builtin := NewMockTool()
	builtin.name = localName
	dispatcher.RegisterTool(builtin)
	client := &MCPClient{
		config: MCPServerConfig{Name: serverName},
		tools: map[string]MCPToolDef{remoteName: {
			Name: remoteName,
			InputSchema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{},
			},
		}},
	}
	manager.clients[serverName] = client
	manager.registered[serverName] = make(map[string]struct{})

	manager.syncTools(serverName, client, client.GetTools())
	health := manager.Health()
	if health.Configured != 1 || health.Connected != 1 || health.Failed != 1 || len(health.Failures) != 1 {
		t.Fatalf("collision health = %+v", health)
	}
	got, ok := dispatcher.registry.Get(localName)
	if !ok || got != builtin {
		t.Fatalf("existing tool was overwritten: %#v", got)
	}

	dispatcher.UnregisterTool(localName)
	manager.syncTools(serverName, client, client.GetTools())
	health = manager.Health()
	if health.Failed != 0 || len(health.Failures) != 0 {
		t.Fatalf("resolved collision remained unhealthy: %+v", health)
	}
	if got, ok := dispatcher.registry.Get(localName); !ok {
		t.Fatal("MCP tool was not registered after collision was removed")
	} else if _, ok := got.(*MCPTool); !ok {
		t.Fatalf("registered tool type = %T, want *MCPTool", got)
	}
}

func TestMCPToolManagerRejectsDuplicateNameEvenAfterFirstConnectionFails(t *testing.T) {
	manager := NewMCPToolManager(NewDispatcherWithRegistry(NewRegistry()))
	config := MCPServerConfig{Name: "duplicate", Transport: "unsupported"}
	if err := manager.Connect(config); err == nil {
		t.Fatal("invalid first server unexpectedly connected")
	}
	err := manager.Connect(config)
	if err == nil || !strings.Contains(err.Error(), "configured more than once") {
		t.Fatalf("duplicate name error = %v", err)
	}
	health := manager.Health()
	if health.Configured != 2 || health.Connected != 0 || health.Failed != 1 || !strings.Contains(health.Failures[0].Error, "configured more than once") {
		t.Fatalf("duplicate health = %+v", health)
	}
}

func newMCPTestServer(pageSize int) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "selfmind-test", Version: "1"}, &mcp.ServerOptions{PageSize: pageSize})
	addMCPTestTool(server, "echo")
	addMCPTestTool(server, "second")
	return server
}

func addMCPTestTool(server *mcp.Server, name string) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: "test tool"}, func(_ context.Context, _ *mcp.CallToolRequest, input mcpEchoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: input.Text}}}, nil, nil
	})
}

func waitForMCPTool(t *testing.T, dispatcher *Dispatcher, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, got := dispatcher.registry.Get(name)
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	names := strings.Join(dispatcher.registry.List(), ", ")
	t.Fatalf("tool %s presence did not become %t; registered: %s", name, want, names)
}
