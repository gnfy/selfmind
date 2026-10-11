// Package mcptest provides a small, deterministic Model Context Protocol
// server for tests that need a real MCP peer: agent-worker tool parity, the
// shared MCP connection, and eval fixtures.
//
// NewServer runs in-process over any SDK transport. The daemon, however,
// reaches configured servers through a stdio child process, so tests of that
// wiring build ./cmd/mcpstub with Build and configure the returned executable
// as an ordinary stdio server. StartLogArgs makes every started stub process
// record its process ID, which lets a test count server processes.
//
// The package depends only on the MCP SDK, so the tools package's own tests
// can import it without an import cycle.
package mcptest

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// ImplementationName is the server name the stub reports during MCP
	// initialization.
	ImplementationName = "selfmind-mcp-stub"
	// StubPackage is the import path of the stdio stub command.
	StubPackage = "selfmind/internal/tools/mcptest/cmd/mcpstub"

	// EchoTool returns its text argument unchanged.
	EchoTool = "echo"
	// RendezvousTool returns only once Parties calls that share a Key are in
	// flight together. Success therefore proves that those calls ran
	// concurrently over the same server session.
	RendezvousTool = "rendezvous"
	// RecordTool appends its text to the server's record, so it changes
	// state, yet its annotations claim it is read-only. A client must not
	// believe a server's description of its own tools.
	RecordTool = "record"

	defaultRendezvousTimeout = 10 * time.Second
)

// EchoInput is the EchoTool argument object.
type EchoInput struct {
	Text string `json:"text" jsonschema:"text to return unchanged"`
}

// RendezvousInput is the RendezvousTool argument object.
type RendezvousInput struct {
	Key       string `json:"key" jsonschema:"meeting point shared by the concurrent calls"`
	Parties   int    `json:"parties" jsonschema:"number of calls that must be in flight together"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"how long to wait for the other calls in milliseconds"`
}

// NewServer returns a fresh stub server. Rendezvous state is per server, so
// every stub process starts with no pending meetings.
func NewServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: ImplementationName, Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: EchoTool, Description: "Return the text argument unchanged."}, echo)
	meetings := &rendezvous{groups: make(map[string]*meeting)}
	mcp.AddTool(server, &mcp.Tool{
		Name:        RendezvousTool,
		Description: "Wait until the given number of calls with the same key are in flight, then return.",
	}, meetings.handle)
	record := &record{}
	mcp.AddTool(server, &mcp.Tool{
		Name:        RecordTool,
		Description: "Append the text to the server's record.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, record.handle)
	return server
}

// record is the RecordTool's server state.
type record struct {
	mu      sync.Mutex
	entries []string
}

func (r *record) handle(_ context.Context, _ *mcp.CallToolRequest, input EchoInput) (*mcp.CallToolResult, any, error) {
	r.mu.Lock()
	r.entries = append(r.entries, input.Text)
	count := len(r.entries)
	r.mu.Unlock()
	return textResult(fmt.Sprintf("recorded %q; the record now has %d entries", input.Text, count)), nil, nil
}

func echo(_ context.Context, _ *mcp.CallToolRequest, input EchoInput) (*mcp.CallToolResult, any, error) {
	return textResult(input.Text), nil, nil
}

type rendezvous struct {
	mu     sync.Mutex
	groups map[string]*meeting
}

type meeting struct {
	arrived int
	met     chan struct{}
}

func (r *rendezvous) handle(ctx context.Context, _ *mcp.CallToolRequest, input RendezvousInput) (*mcp.CallToolResult, any, error) {
	key := strings.TrimSpace(input.Key)
	if key == "" || input.Parties < 1 {
		return nil, nil, fmt.Errorf("rendezvous needs a key and a positive parties count")
	}
	timeout := time.Duration(input.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultRendezvousTimeout
	}
	r.mu.Lock()
	group := r.groups[key]
	if group == nil {
		group = &meeting{met: make(chan struct{})}
		r.groups[key] = group
	}
	group.arrived++
	if group.arrived == input.Parties {
		close(group.met)
	}
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-group.met:
		return textResult(fmt.Sprintf("rendezvous %s: %d parties met", key, input.Parties)), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-timer.C:
		r.mu.Lock()
		arrived := group.arrived
		r.mu.Unlock()
		return nil, nil, fmt.Errorf("rendezvous %s: %d of %d parties arrived within %s", key, arrived, input.Parties, timeout)
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// Main serves the stub over stdin/stdout until the client disconnects. args
// are the command-line arguments after the program name. With -start-log, the
// process appends its ID to that file before serving.
func Main(args []string) error {
	flags := flag.NewFlagSet("mcpstub", flag.ContinueOnError)
	startLog := flags.String("start-log", "", "append this process ID to the file before serving")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if path := strings.TrimSpace(*startLog); path != "" {
		if err := appendLine(path, strconv.Itoa(os.Getpid())); err != nil {
			return fmt.Errorf("record start: %w", err)
		}
	}
	session, err := NewServer().Connect(context.Background(), &mcp.StdioTransport{}, nil)
	if err != nil {
		return err
	}
	// The session ends when the client disconnects. The SDK reports that as
	// EOF or as a "server is closing" error depending on timing; either way
	// it is the stub's normal end of life, and the client sees any protocol
	// failure itself.
	_ = session.Wait()
	return nil
}

func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(file, line); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// processHome is HOME as the test process started, before any test points
// HOME at a temporary directory.
var processHome = os.Getenv("HOME")

// Build compiles the stdio stub into the test's temporary directory and
// returns the executable path. `go test` places its own Go tool first on PATH,
// so the stub is built by the same toolchain as the test. The build uses the
// process's original HOME: the Go tool derives its module and build caches
// from it, and a test that isolates HOME would otherwise download every
// module into a fresh cache its cleanup cannot remove.
func Build(t testing.TB) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("build MCP stub: Go tool not found: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mcpstub")
	cmd := exec.Command(goTool, "build", "-o", path, StubPackage)
	cmd.Env = append(os.Environ(), "HOME="+processHome)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build MCP stub: %v\n%s", err, output)
	}
	return path
}

// StartLogArgs returns the stub arguments that record every started stub
// process in path.
func StartLogArgs(path string) []string {
	return []string{"-start-log", path}
}

// Starts returns the process IDs recorded in a start log, one per started
// stub process. A missing log means no process has started.
func Starts(t testing.TB, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read MCP stub start log: %v", err)
	}
	defer file.Close()
	var starts []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			starts = append(starts, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read MCP stub start log: %v", err)
	}
	return starts
}
