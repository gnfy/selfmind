package mcptest_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"selfmind/internal/tools/mcptest"
)

// The stub must behave as a real stdio MCP server: one process per
// connection, recorded in its start log, serving echo and a rendezvous that
// only completes while both calls are in flight on the same session.
func TestStdioStubServesEchoAndConcurrentRendezvous(t *testing.T) {
	stub := mcptest.Build(t)
	startLog := filepath.Join(t.TempDir(), "starts.log")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "mcptest-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command: exec.Command(stub, mcptest.StartLogArgs(startLog)...),
	}, nil)
	if err != nil {
		t.Fatalf("connect stub: %v", err)
	}
	defer session.Close()

	if got := session.InitializeResult().ServerInfo.Name; got != mcptest.ImplementationName {
		t.Fatalf("server name = %q, want %q", got, mcptest.ImplementationName)
	}
	echoed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: mcptest.EchoTool, Arguments: map[string]any{"text": "stub reached"}})
	if err != nil || echoed.IsError || resultText(echoed) != "stub reached" {
		t.Fatalf("echo = %q (error result %v), err=%v", resultText(echoed), echoed != nil && echoed.IsError, err)
	}

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, callErr := session.CallTool(ctx, &mcp.CallToolParams{Name: mcptest.RendezvousTool,
				Arguments: map[string]any{"key": "pair", "parties": 2, "timeout_ms": 5000}})
			errs[i] = callErr
			if res != nil && res.IsError {
				errs[i] = errFromResult(res)
			}
			results[i] = resultText(res)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !strings.Contains(results[i], "2 parties met") {
			t.Fatalf("rendezvous call %d = %q, err=%v", i, results[i], errs[i])
		}
	}

	if starts := mcptest.Starts(t, startLog); len(starts) != 1 {
		t.Fatalf("stub processes started = %d (%v), want 1", len(starts), starts)
	}
}

// A lone rendezvous call must fail rather than report a meeting that never
// happened; otherwise a serialized client would pass a concurrency test.
func TestRendezvousFailsWithoutTheOtherParty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcptest.NewServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "mcptest-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: mcptest.RendezvousTool,
		Arguments: map[string]any{"key": "alone", "parties": 2, "timeout_ms": 50}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "1 of 2 parties") {
		t.Fatalf("lone rendezvous = %q (error result %v)", resultText(res), res.IsError)
	}
}

// The record tool is the stub's untrustworthy self-report: it changes the
// server's state while its annotations claim it is read-only.
func TestRecordToolWritesWhileClaimingReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcptest.NewServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "mcptest-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimsReadOnly := false
	for _, tool := range listed.Tools {
		if tool.Name == mcptest.RecordTool {
			claimsReadOnly = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		}
	}
	if !claimsReadOnly {
		t.Fatalf("record tool does not claim readOnlyHint: %+v", listed.Tools)
	}
	for i, want := range []string{"1 entries", "2 entries"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: mcptest.RecordTool, Arguments: map[string]any{"text": "change"}})
		if err != nil || res.IsError || !strings.Contains(resultText(res), want) {
			t.Fatalf("record call %d = %q, err=%v; want %q", i+1, resultText(res), err, want)
		}
	}
}

func resultText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type resultError string

func (e resultError) Error() string { return string(e) }

func errFromResult(res *mcp.CallToolResult) error {
	return resultError(resultText(res))
}
