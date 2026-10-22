package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"selfmind/internal/tools/mcptest"
)

// Every agent worker owns a dispatcher. One MCP connection must serve all of
// them: each dispatcher registers the same tools, bound to the same client,
// and a catalogue change reaches every dispatcher in the same sync.
func TestMCPToolManagerKeepsEveryDispatcherOnOneSharedClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcptest.NewServer()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	primary := NewDispatcherWithRegistry(NewRegistry())
	worker := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(primary, worker)
	config := MCPServerConfig{Name: "shared", Transport: "in-memory"}
	client, err := newMCPClientWithTransport(ctx, config, clientTransport, func(changed *MCPClient, defs map[string]MCPToolDef) {
		manager.syncTools(config.Name, changed, defs)
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.clients[config.Name] = client
	manager.registered[config.Name] = make(map[string]struct{})
	manager.syncTools(config.Name, client, client.GetTools())

	echoName := MCPToolLocalName(config.Name, mcptest.EchoTool)
	for index, dispatcher := range []*Dispatcher{primary, worker} {
		got, ok := dispatcher.registry.Get(echoName)
		if !ok {
			t.Fatalf("dispatcher %d lacks %s; registered: %v", index, echoName, dispatcher.ListTools())
		}
		if tool, ok := got.(*MCPTool); !ok || tool.client != client {
			t.Fatalf("dispatcher %d tool %s is not bound to the shared client: %#v", index, echoName, got)
		}
	}

	lateName := MCPToolLocalName(config.Name, "late")
	addLateTool(server)
	waitForMCPTool(t, primary, lateName, true)
	waitForMCPTool(t, worker, lateName, true)

	server.RemoveTools(mcptest.EchoTool)
	waitForMCPTool(t, primary, echoName, false)
	waitForMCPTool(t, worker, echoName, false)

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for index, dispatcher := range []*Dispatcher{primary, worker} {
		for _, name := range dispatcher.ListTools() {
			if strings.HasPrefix(name, "mcp_") {
				t.Fatalf("dispatcher %d kept %s after the manager closed", index, name)
			}
		}
	}
}

// A local-name collision in any one dispatcher keeps the tool out of all of
// them. Registering it only where it fits would give workers different
// catalogues, which is exactly the drift the shared manager exists to prevent.
func TestMCPToolManagerCollisionInOneDispatcherRegistersInNone(t *testing.T) {
	primary := NewDispatcherWithRegistry(NewRegistry())
	worker := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(primary, worker)
	manager.configured = 1
	serverName, remoteName := "external", "search"
	localName := MCPToolLocalName(serverName, remoteName)
	builtin := NewMockTool()
	builtin.name = localName
	worker.RegisterTool(builtin)
	client := &MCPClient{
		config: MCPServerConfig{Name: serverName},
		tools: map[string]MCPToolDef{remoteName: {Name: remoteName, InputSchema: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{},
		}}},
	}
	manager.clients[serverName] = client
	manager.registered[serverName] = make(map[string]struct{})

	manager.syncTools(serverName, client, client.GetTools())
	if _, ok := primary.registry.Get(localName); ok {
		t.Fatalf("%s was registered in the primary although it collides in another worker", localName)
	}
	if got, _ := worker.registry.Get(localName); got != builtin {
		t.Fatalf("colliding worker tool was replaced: %#v", got)
	}
	if health := manager.Health(); health.Failed != 1 || !strings.Contains(health.Failures[0].Error, localName) {
		t.Fatalf("collision was not reported: %+v", health)
	}
}

// The race detector run of this test is the evidence that two workers may
// call the same MCP server at once: one stdio process serves both dispatchers,
// and the rendezvous only succeeds while both calls are in flight together.
func TestMCPToolManagerServesParallelCallsFromTwoDispatchersOverOneProcess(t *testing.T) {
	stub := mcptest.Build(t)
	startLog := filepath.Join(t.TempDir(), "starts.log")
	primary := NewDispatcherWithRegistry(NewRegistry())
	worker := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(primary, worker)
	defer manager.Close()
	if err := manager.Connect(MCPServerConfig{Name: "parallel", Transport: "stdio", Command: stub, Args: mcptest.StartLogArgs(startLog)}); err != nil {
		t.Fatal(err)
	}

	name := MCPToolLocalName("parallel", mcptest.RendezvousTool)
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for index, dispatcher := range []*Dispatcher{primary, worker} {
		wg.Add(1)
		go func(index int, dispatcher *Dispatcher) {
			defer wg.Done()
			results[index], errs[index] = dispatcher.Dispatch(name, map[string]interface{}{
				"key": "two-workers", "parties": 2, "timeout_ms": 5000,
			})
		}(index, dispatcher)
	}
	wg.Wait()
	for index := range results {
		if errs[index] != nil || !strings.Contains(results[index], "2 parties met") {
			t.Fatalf("dispatcher %d rendezvous = %q, err=%v", index, results[index], errs[index])
		}
	}
	if starts := mcptest.Starts(t, startLog); len(starts) != 1 {
		t.Fatalf("MCP server processes = %d (%v), want one shared process", len(starts), starts)
	}
	if health := manager.Health(); health.Configured != 1 || health.Connected != 1 || health.Failed != 0 {
		t.Fatalf("health = %+v", health)
	}
}

// The same shared instance is read when two workers build their first
// provider catalogue at the same moment. That path holds only a registry read
// lock, so the shared tool must not be mutated by reading it.
func TestSharedMCPToolCatalogueBuildsConcurrentlyInTwoWorkers(t *testing.T) {
	primary := NewDispatcherWithRegistry(NewRegistry())
	worker := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(primary, worker)
	client := &MCPClient{
		config: MCPServerConfig{Name: "catalogue"},
		tools: map[string]MCPToolDef{"lookup": {Name: "lookup", Description: "look something up", InputSchema: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}},
		}}},
	}
	manager.clients["catalogue"] = client
	manager.registered["catalogue"] = make(map[string]struct{})
	manager.syncTools("catalogue", client, client.GetTools())

	name := MCPToolLocalName("catalogue", "lookup")
	var wg sync.WaitGroup
	found := make([]bool, 2)
	for index, dispatcher := range []*Dispatcher{primary, worker} {
		wg.Add(1)
		go func(index int, dispatcher *Dispatcher) {
			defer wg.Done()
			for _, definition := range dispatcher.GetToolDefinitions() {
				if function, _ := definition["function"].(map[string]interface{}); function["name"] == name {
					found[index] = true
				}
			}
		}(index, dispatcher)
	}
	wg.Wait()
	if !found[0] || !found[1] {
		t.Fatalf("shared MCP tool missing from a worker catalogue: %v", found)
	}
}

// HoldCatalog promises fn the same MCP tools in every dispatcher, so removing
// a server's tools belongs inside the hold as well: fn may run before or after
// a Disconnect or Close, never while one dispatcher has lost a tool that
// another still offers.
func TestMCPToolManagerHoldCatalogNeverSeesAHalfRemovedServer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(*MCPToolManager) error
	}{
		{"disconnect", func(m *MCPToolManager) error { return m.Disconnect("held") }},
		{"close", func(m *MCPToolManager) error { return m.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := NewDispatcherWithRegistry(NewRegistry())
			worker := NewDispatcherWithRegistry(NewRegistry())
			manager := NewMCPToolManager(primary, worker)
			client := &MCPClient{
				config: MCPServerConfig{Name: "held"},
				tools: map[string]MCPToolDef{"lookup": {Name: "lookup", InputSchema: map[string]interface{}{
					"type": "object", "properties": map[string]interface{}{},
				}}},
			}
			manager.clients["held"] = client
			manager.registered["held"] = make(map[string]struct{})
			manager.syncTools("held", client, client.GetTools())
			name := MCPToolLocalName("held", "lookup")

			// Stall the removal at the worker after the primary has already
			// lost the tool: the moment no hold may observe.
			worker.registry.mu.Lock()
			var unlockOnce sync.Once
			unlockWorker := func() { unlockOnce.Do(worker.registry.mu.Unlock) }
			defer unlockWorker()
			removed := make(chan error, 1)
			go func() { removed <- tc.remove(manager) }()
			waitForMCPTool(t, primary, name, false)

			held := make(chan struct{})
			go manager.HoldCatalog(func() { close(held) })
			select {
			case <-held:
				unlockWorker()
				t.Fatalf("HoldCatalog ran while %s was gone from the primary but still registered in the worker", name)
			case <-time.After(100 * time.Millisecond):
			}
			unlockWorker()

			select {
			case <-held:
			case <-time.After(2 * time.Second):
				t.Fatal("HoldCatalog did not run after the removal finished")
			}
			if err := <-removed; err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for index, dispatcher := range []*Dispatcher{primary, worker} {
				if dispatcher.ToolExists(name) {
					t.Fatalf("dispatcher %d kept %s after %s", index, name, tc.name)
				}
			}
		})
	}
}

func addLateTool(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "late", Description: "late tool"}, func(_ context.Context, _ *mcp.CallToolRequest, input mcptest.EchoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprint(input.Text)}}}, nil, nil
	})
}
