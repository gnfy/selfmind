package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"selfmind/internal/buildinfo"
	"selfmind/internal/platform/log"
)

const (
	mcpStartupTimeout = 30 * time.Second
	mcpCatalogTimeout = 30 * time.Second
	mcpToolTimeout    = 60 * time.Second
	mcpMaxListPages   = 1000
)

// MCPServerConfig describes one external MCP server.
type MCPServerConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // stdio or http/streamable_http
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Auth      map[string]string `json:"auth,omitempty"`
	EnvFilter []string          `json:"env_filter,omitempty"`
}

// MCPToolDef is the provider-neutral subset of an SDK tool definition used by
// SelfMind's registry and schema compiler.
type MCPToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type mcpToolsChanged func(*MCPClient, map[string]MCPToolDef)

// MCPClient owns one initialized official-SDK session and its current tool
// catalogue. Transport framing, protocol negotiation, sessions, SSE, and
// notifications remain SDK responsibilities.
type MCPClient struct {
	config  MCPServerConfig
	client  *mcp.Client
	session *mcp.ClientSession

	toolsMu sync.RWMutex
	tools   map[string]MCPToolDef
	onTools mcpToolsChanged

	closeOnce sync.Once
	closeErr  error
}

func NewMCPClient(config MCPServerConfig) (*MCPClient, error) {
	return newMCPClient(config, nil)
}

func newMCPClient(config MCPServerConfig, onTools mcpToolsChanged) (*MCPClient, error) {
	transport, err := newMCPTransport(config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpStartupTimeout)
	defer cancel()
	return newMCPClientWithTransport(ctx, config, transport, onTools)
}

func newMCPClientWithTransport(ctx context.Context, config MCPServerConfig, transport mcp.Transport, onTools mcpToolsChanged) (*MCPClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := &MCPClient{
		config:  config,
		tools:   make(map[string]MCPToolDef),
		onTools: onTools,
	}
	options := &mcp.ClientOptions{
		// MCP servers do not receive implicit filesystem roots. Any filesystem
		// authority must continue to flow through SelfMind's typed tool scope.
		Capabilities: &mcp.ClientCapabilities{},
		ToolListChangedHandler: func(handlerCtx context.Context, _ *mcp.ToolListChangedRequest) {
			refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(handlerCtx), mcpCatalogTimeout)
			defer cancel()
			if err := client.refreshTools(refreshCtx, true); err != nil {
				log.Warn("mcp: tool catalogue refresh failed", "name", config.Name, "error", err)
			}
		},
	}
	client.client = mcp.NewClient(&mcp.Implementation{
		Name:    "selfmind",
		Version: buildinfo.Version,
	}, options)

	session, err := client.client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP server %s: %w", config.Name, err)
	}
	client.session = session
	if err := client.refreshTools(ctx, false); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("list MCP tools for %s: %w", config.Name, err)
	}
	return client, nil
}

func newMCPTransport(config MCPServerConfig) (mcp.Transport, error) {
	switch strings.ToLower(strings.TrimSpace(config.Transport)) {
	case "stdio":
		if strings.TrimSpace(config.Command) == "" {
			return nil, fmt.Errorf("stdio transport requires 'command' field")
		}
		cmd := exec.Command(config.Command, config.Args...)
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve MCP working directory: %w", err)
		}
		cmd.Dir = cwd
		cmd.Env = BuildProcessEnv(filterEnv(config.EnvFilter), DefaultProcessEnvPolicy())
		return &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, nil
	case "http", "streamable_http", "streamable-http":
		if strings.TrimSpace(config.URL) == "" {
			return nil, fmt.Errorf("streamable HTTP transport requires 'url' field")
		}
		return &mcp.StreamableClientTransport{
			Endpoint: strings.TrimSpace(config.URL),
			HTTPClient: &http.Client{Transport: &mcpHeaderTransport{
				base:    http.DefaultTransport,
				headers: mcpHTTPHeaders(config),
			}},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported MCP transport: %s", config.Transport)
	}
}

type mcpHeaderTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *mcpHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for key, values := range t.headers {
		clone.Header.Del(key)
		for _, value := range values {
			clone.Header.Add(key, value)
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func mcpHTTPHeaders(config MCPServerConfig) http.Header {
	headers := make(http.Header, len(config.Headers)+1)
	for key, value := range config.Headers {
		headers.Set(key, value)
	}
	if token := strings.TrimSpace(config.Auth["bearer"]); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	} else {
		user := config.Auth["user"]
		pass := config.Auth["pass"]
		if user != "" && pass != "" {
			encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			headers.Set("Authorization", "Basic "+encoded)
		}
	}
	return headers
}

func (c *MCPClient) refreshTools(ctx context.Context, notify bool) error {
	if c.session == nil {
		return fmt.Errorf("MCP session is not connected")
	}
	tools := make(map[string]MCPToolDef)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 0; page < mcpMaxListPages; page++ {
		result, err := c.session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return err
		}
		for _, tool := range result.Tools {
			if tool == nil || strings.TrimSpace(tool.Name) == "" {
				continue
			}
			schema, err := mcpSchemaMap(tool.InputSchema)
			if err != nil {
				return fmt.Errorf("tool %s input schema: %w", tool.Name, err)
			}
			tools[tool.Name] = MCPToolDef{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			}
		}
		if result.NextCursor == "" {
			break
		}
		if _, duplicate := seenCursors[result.NextCursor]; duplicate {
			return fmt.Errorf("tools/list returned duplicate cursor %q", result.NextCursor)
		}
		seenCursors[result.NextCursor] = struct{}{}
		cursor = result.NextCursor
		if page == mcpMaxListPages-1 {
			return fmt.Errorf("tools/list exceeded %d pages", mcpMaxListPages)
		}
	}

	c.toolsMu.Lock()
	changed := !reflect.DeepEqual(c.tools, tools)
	c.tools = tools
	onTools := c.onTools
	c.toolsMu.Unlock()
	if notify && changed && onTools != nil {
		onTools(c, c.GetTools())
	}
	return nil
}

func mcpSchemaMap(schema any) (map[string]interface{}, error) {
	if schema == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	return result, nil
}

func (c *MCPClient) CallTool(name string, args map[string]interface{}) (string, error) {
	c.toolsMu.RLock()
	_, ok := c.tools[name]
	c.toolsMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tool %s not found on server %s", name, c.config.Name)
	}
	if c.session == nil {
		return "", fmt.Errorf("MCP server %s is not connected", c.config.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpToolTimeout)
	defer cancel()
	// The dispatcher carries authenticated scope, registry pointers, callbacks,
	// and other daemon-only values in underscore-prefixed arguments. They are
	// useful to local middleware but must never cross the MCP trust boundary.
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: publicToolArgs(args)})
	if err != nil {
		return "", err
	}
	output := renderMCPToolResult(result)
	if result.IsError {
		if strings.TrimSpace(output) == "" {
			output = "MCP tool returned an error"
		}
		return "", fmt.Errorf("tool %s: %s", name, output)
	}
	return output, nil
}

func renderMCPToolResult(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	parts := make([]string, 0, len(result.Content)+1)
	for _, content := range result.Content {
		switch item := content.(type) {
		case *mcp.TextContent:
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		case *mcp.ImageContent:
			parts = append(parts, mediaPlaceholder("Image", item.MIMEType))
		case *mcp.AudioContent:
			parts = append(parts, mediaPlaceholder("Audio", item.MIMEType))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[Resource: %s]", item.URI))
		case *mcp.EmbeddedResource:
			if item.Resource == nil {
				continue
			}
			if item.Resource.Text != "" {
				parts = append(parts, item.Resource.Text)
			} else {
				parts = append(parts, fmt.Sprintf("[Resource: %s]", item.Resource.URI))
			}
		default:
			if data, err := json.Marshal(content); err == nil {
				parts = append(parts, string(data))
			}
		}
	}
	if len(parts) == 0 && result.StructuredContent != nil {
		if data, err := json.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, string(data))
		}
	}
	return strings.Join(parts, "\n")
}

func mediaPlaceholder(kind, mimeType string) string {
	if strings.TrimSpace(mimeType) == "" {
		return "[" + kind + "]"
	}
	return fmt.Sprintf("[%s: %s]", kind, mimeType)
}

func (c *MCPClient) GetTools() map[string]MCPToolDef {
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	result := make(map[string]MCPToolDef, len(c.tools))
	for name, tool := range c.tools {
		result[name] = tool
	}
	return result
}

func (c *MCPClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.session != nil {
			c.closeErr = c.session.Close()
		}
	})
	return c.closeErr
}
