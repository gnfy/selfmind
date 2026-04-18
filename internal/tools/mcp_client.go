package tools

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MCPServerConfig MCP 服务器配置
type MCPServerConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // "stdio" or "http"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Auth      map[string]string `json:"auth,omitempty"`
	EnvFilter []string          `json:"env_filter,omitempty"`
}

// MCPToolDef 从 MCP 服务器发现的一个工具
type MCPToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// MCPClient MCP 客户端管理器
type MCPClient struct {
	mu      sync.RWMutex
	config  MCPServerConfig
	server  *mcpServer
	tools   map[string]MCPToolDef
	toolsMu sync.RWMutex
	backoff time.Duration
}

type mcpServer struct {
	name      string
	cmd       *exec.Cmd
	stdin     *os.File
	stdout    *os.File
	url       string
	headers   map[string]string
	transport string
	tools     map[string]MCPToolDef
	errCh     chan error
	done      chan struct{}
}

func NewMCPClient(config MCPServerConfig) (*MCPClient, error) {
	client := &MCPClient{
		config: config,
		tools:  make(map[string]MCPToolDef),
	}
	if err := client.connect(); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *MCPClient) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Apply exponential backoff if we just failed
	if c.backoff > 0 {
		time.Sleep(c.backoff)
	}

	if c.server != nil && c.server.transport == "stdio" && c.server.cmd != nil && c.server.cmd.ProcessState == nil {
		return nil // Still running
	}

	switch c.config.Transport {
	case "stdio":
		return c.connectStdio()
	case "http":
		return c.connectHTTP()
	default:
		return fmt.Errorf("unsupported transport: %s", c.config.Transport)
	}
}

// ---- Stdio Transport ----

func (c *MCPClient) connectStdio() error {
	if c.config.Command == "" {
		return fmt.Errorf("stdio transport requires 'command' field")
	}

	// Reset backoff on successful connect
	defer func() {
		if c.backoff == 0 {
			c.backoff = 500 * time.Millisecond
		}
	}()

	cmd := exec.Command(c.config.Command, c.config.Args...)
	cmd.Dir, _ = os.Getwd()
	cmd.Env = filterEnv(c.config.EnvFilter)

	parentStdin, childStdin, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	parentStdout, childStdout, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}

	cmd.Stdin = childStdin
	cmd.Stdout = childStdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	c.server = &mcpServer{
		name:      c.config.Name,
		cmd:       cmd,
		stdin:     parentStdin,
		stdout:    parentStdout,
		transport: "stdio",
		tools:     make(map[string]MCPToolDef),
		errCh:     make(chan error, 1),
		done:      make(chan struct{}),
	}

	go c.stdioReader(parentStdout)
	c.initialize()
	return nil
}

func (c *MCPClient) stdioReader(stdout *os.File) {
	defer close(c.server.done)
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		c.handleMessage(&msg)
	}
	if err := scanner.Err(); err != nil && c.server.errCh != nil {
		c.server.errCh <- fmt.Errorf("stdio read: %w", err)
	}
}

// ---- HTTP Transport ----

func (c *MCPClient) connectHTTP() error {
	if c.config.URL == "" {
		return fmt.Errorf("http transport requires 'url' field")
	}
	c.server = &mcpServer{
		name:      c.config.Name,
		url:       c.config.URL,
		headers:   c.config.Headers,
		transport: "http",
		tools:     make(map[string]MCPToolDef),
	}
	c.initialize()
	return nil
}

// ---- Message Handling ----

func (c *MCPClient) handleMessage(msg *JSONRPCMessage) {
	switch msg.Method {
	case "notifications/tools/list_changed":
		c.listTools()
	case "sampling/createMessage":
		c.sendError(msg.ID, -1, "sampling not implemented")
	default:
		if msg.ID != nil {
			id := int64(msg.ID.(float64))
			getResponseChan(id) <- msg
		}
	}
}

// ---- Protocol Operations ----

func (c *MCPClient) initialize() {
	_, err := c.sendRequest("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools":    map[string]interface{}{},
			"sampling": map[string]interface{}{},
			"roots":    map[string]interface{}{},
		},
		"clientInfo": map[string]string{
			"name":    "selfmind",
			"version": "1.0.0",
		},
	}, nil)
	if err != nil && c.server != nil && c.server.errCh != nil {
		c.server.errCh <- fmt.Errorf("initialize: %w", err)
		return
	}
	c.sendNotification("initialized", nil)
	c.listTools()
}

func (c *MCPClient) listTools() {
	var result struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"inputSchema"`
		} `json:"tools"`
	}

	var err error
	switch c.server.transport {
	case "stdio":
		_, err = c.sendRequest("tools/list", nil, &result)
	case "http":
		_, err = c.callHTTP("tools/list", nil, &result)
	}
	if err != nil {
		return
	}

	c.toolsMu.Lock()
	c.tools = make(map[string]MCPToolDef)
	for _, t := range result.Tools {
		c.tools[t.Name] = MCPToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	c.toolsMu.Unlock()
}

func (c *MCPClient) CallTool(name string, args map[string]interface{}) (string, error) {
	c.toolsMu.RLock()
	_, ok := c.tools[name]
	c.toolsMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tool %s not found on server %s", name, c.config.Name)
	}

	params := map[string]interface{}{
		"name":      name,
		"arguments": args,
	}

	var result struct {
		Content []map[string]interface{} `json:"content"`
		IsError bool                     `json:"isError"`
	}

	var err error
	switch c.server.transport {
	case "stdio":
		_, err = c.sendRequest("tools/call", params, &result)
	case "http":
		_, err = c.callHTTP("tools/call", params, &result)
	}
	if err != nil {
		return "", err
	}
	if result.IsError {
		return "", fmt.Errorf("tool error")
	}

	var output strings.Builder
	for _, item := range result.Content {
		switch item["type"] {
		case "text":
			if text, ok := item["text"].(string); ok {
				output.WriteString(text)
			}
		case "image":
			output.WriteString("[Image]")
		case "resource":
			if uri, ok := item["uri"].(string); ok {
				output.WriteString(fmt.Sprintf("[Resource: %s]", uri))
			}
		}
	}
	return output.String(), nil
}

func (c *MCPClient) GetTools() map[string]MCPToolDef {
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	result := make(map[string]MCPToolDef)
	for k, v := range c.tools {
		result[k] = v
	}
	return result
}

func (c *MCPClient) Close() error {
	if c.server == nil {
		return nil
	}
	if c.server.transport == "stdio" && c.server.cmd != nil && c.server.cmd.Process != nil {
		c.server.cmd.Process.Kill()
		c.server.cmd.Wait()
		c.server.stdin.Close()
		c.server.stdout.Close()
	}
	return nil
}

// =============================================================================
// JSON-RPC Types
// =============================================================================

type JSONRPCMessage struct {
	ID      interface{}    `json:"id,omitempty"`
	JSONRPC string        `json:"jsonrpc"`
	Method  string         `json:"method,omitempty"`
	Params  interface{}    `json:"params,omitempty"`
	Result  interface{}    `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// =============================================================================
// Stdio Request/Response
// =============================================================================

var (
	respMu    sync.Mutex
	respChans = make(map[int64]chan *JSONRPCMessage)
	nextReqID int64
	reqIDMu   sync.Mutex
)

func nextRequestID() int64 {
	reqIDMu.Lock()
	defer reqIDMu.Unlock()
	nextReqID++
	return nextReqID
}

func getResponseChan(id int64) chan *JSONRPCMessage {
	respMu.Lock()
	defer respMu.Unlock()
	if _, exists := respChans[id]; !exists {
		respChans[id] = make(chan *JSONRPCMessage, 1)
	}
	return respChans[id]
}

func (c *MCPClient) sendRequest(method string, params interface{}, result interface{}) (*JSONRPCMessage, error) {
	id := nextRequestID()
	respCh := getResponseChan(id)

	msg := JSONRPCMessage{
		ID:      id,
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	if c.server == nil || (c.server.transport == "stdio" && c.server.cmd.ProcessState != nil) {
		// Try to reconnect
		if err := c.connect(); err != nil {
			// Increase backoff on failure
			c.mu.Lock()
			if c.backoff < 30*time.Second {
				c.backoff *= 2
			}
			c.mu.Unlock()
			return nil, fmt.Errorf("reconnect failed: %w", err)
		}
		// Success! Reset backoff
		c.mu.Lock()
		c.backoff = 0
		c.mu.Unlock()
	}

	if _, err := c.server.stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-respCh:
		respMu.Lock()
		delete(respChans, id)
		respMu.Unlock()
		if resp.Error != nil {
			return resp, fmt.Errorf("json-rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if result != nil && resp.Result != nil {
			bytes, _ := json.Marshal(resp.Result)
			json.Unmarshal(bytes, result)
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		respMu.Lock()
		delete(respChans, id)
		respMu.Unlock()
		return nil, fmt.Errorf("request timeout")
	}
}

func (c *MCPClient) sendNotification(method string, params interface{}) {
	msg := JSONRPCMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(msg)
	c.server.stdin.Write(append(data, '\n'))
}

func (c *MCPClient) sendError(id interface{}, code int, message string) {
	msg := JSONRPCMessage{
		ID:      id,
		JSONRPC: "2.0",
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
	data, _ := json.Marshal(msg)
	c.server.stdin.Write(append(data, '\n'))
}

// ---- HTTP Transport ----

func (c *MCPClient) callHTTP(method string, params interface{}, result interface{}) (*JSONRPCMessage, error) {
	id := nextRequestID()
	msg := JSONRPCMessage{
		ID:      id,
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(msg)

	req, err := http.NewRequest("POST", c.server.url, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.server.headers {
		req.Header.Set(k, v)
	}
	if c.config.Auth != nil {
		if token, ok := c.config.Auth["bearer"]; ok {
			req.Header.Set("Authorization", "Bearer "+token)
		} else if user, pass := c.config.Auth["user"], c.config.Auth["pass"]; user != "" && pass != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		}
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp JSONRPCMessage
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if rpcResp.Error != nil {
		return &rpcResp, fmt.Errorf("json-rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if result != nil && rpcResp.Result != nil {
		bytes, _ := json.Marshal(rpcResp.Result)
		json.Unmarshal(bytes, result)
	}
	return &rpcResp, nil
}

// =============================================================================
// MCPTool - MCP 工具的本地包装器
// =============================================================================

type MCPTool struct {
	BaseTool
	serverName  string
	toolName    string
	inputSchema map[string]interface{}
	client      *MCPClient
}

func (c *MCPClient) WrapTool(def MCPToolDef) *MCPTool {
	return &MCPTool{
		BaseTool: BaseTool{
			name:        def.Name,
			description: def.Description,
			schema:      convertJSONSchema(def.InputSchema),
		},
		serverName:  c.config.Name,
		toolName:    def.Name,
		inputSchema: def.InputSchema,
		client:      c,
	}
}

func (t *MCPTool) Execute(args map[string]interface{}) (string, error) {
	if required, ok := t.inputSchema["required"].([]interface{}); ok {
		for _, r := range required {
			if name, ok := r.(string); ok {
				if _, exists := args[name]; !exists {
					return "", fmt.Errorf("missing required parameter: %s", name)
				}
			}
		}
	}
	return t.client.CallTool(t.toolName, args)
}

func convertJSONSchema(input map[string]interface{}) ToolSchema {
	props := make(map[string]PropertyDef)
	var required []string

	if propsRaw, ok := input["properties"].(map[string]interface{}); ok {
		for name, propRaw := range propsRaw {
			if prop, ok := propRaw.(map[string]interface{}); ok {
				p := PropertyDef{}
				if t, ok := prop["type"].(string); ok {
					p.Type = t
				}
				if desc, ok := prop["description"].(string); ok {
					p.Description = desc
				}
				if e, ok := prop["enum"].([]interface{}); ok {
					for _, v := range e {
						p.Enum = append(p.Enum, fmt.Sprintf("%v", v))
					}
				}
				props[name] = p
			}
		}
	}

	if req, ok := input["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}

	return ToolSchema{
		Type:       "object",
		Properties: props,
		Required:   required,
	}
}

// =============================================================================
// MCP Tool Manager
// =============================================================================

type MCPToolManager struct {
	mu         sync.RWMutex
	clients    map[string]*MCPClient
	dispatcher *Dispatcher
}

func NewMCPToolManager(d *Dispatcher) *MCPToolManager {
	return &MCPToolManager{
		clients:    make(map[string]*MCPClient),
		dispatcher: d,
	}
}

func (m *MCPToolManager) Connect(config MCPServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.clients[config.Name]; exists {
		return fmt.Errorf("server %s already connected", config.Name)
	}

	client, err := NewMCPClient(config)
	if err != nil {
		return fmt.Errorf("connect %s: %w", config.Name, err)
	}

	m.clients[config.Name] = client
	time.Sleep(500 * time.Millisecond)

	for _, def := range client.GetTools() {
		m.dispatcher.RegisterTool(client.WrapTool(def))
	}
	return nil
}

func (m *MCPToolManager) Disconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, ok := m.clients[name]
	if !ok {
		return fmt.Errorf("server %s not found", name)
	}

	for _, def := range client.GetTools() {
		// Unregister from dispatcher - use the registry directly
		globalRegistry.Unregister(def.Name)
	}

	client.Close()
	delete(m.clients, name)
	return nil
}

func (m *MCPToolManager) ListServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	return names
}

func (m *MCPToolManager) ListTools(serverName string) []MCPToolDef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[serverName]
	if !ok {
		return nil
	}
	tools := client.GetTools()
	result := make([]MCPToolDef, 0, len(tools))
	for _, t := range tools {
		result = append(result, t)
	}
	return result
}

// =============================================================================
// Env Filtering
// =============================================================================

func filterEnv(whitelist []string) []string {
	if len(whitelist) == 0 {
		whitelist = []string{"PATH", "HOME", "USER", "TMPDIR"}
	}
	whitelistMap := make(map[string]struct{})
	for _, k := range whitelist {
		whitelistMap[k] = struct{}{}
	}

	var result []string
	for _, e := range os.Environ() {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if _, allowed := whitelistMap[key]; allowed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "secret") || strings.Contains(lower, "token") ||
				strings.Contains(lower, "password") || strings.Contains(lower, "key") {
				continue
			}
			result = append(result, e)
		}
	}
	return result
}

// =============================================================================
// Utility
// =============================================================================

// sanitizeFTS5Query 清理 FTS5 查询特殊字符
func sanitizeFTS5Query(query string) string {
	phraseRe := regexp.MustCompile(`"([^"]+)"`)
	phrases := phraseRe.FindAllStringSubmatch(query, -1)

	s := query
	special := []string{"+", "{", "}", "(", ")", "^", "\"", ":", "*", "?"}
	for _, c := range special {
		s = strings.ReplaceAll(s, c, " ")
	}
	s = strings.TrimSpace(s)

	for _, m := range phrases {
		if len(m) > 1 {
			s = s + " \"" + m[1] + "\""
		}
	}

	wordRe := regexp.MustCompile(`\b[\w\-\.]+\b`)
	words := wordRe.FindAllString(s, -1)
	for _, w := range words {
		if strings.Contains(w, "-") || strings.Contains(w, ".") {
			s = strings.ReplaceAll(s, w, "\""+w+"\"")
		}
	}
	return s
}

// ---- Fix dispatcher.Unregister -> globalRegistry.Unregister ----
// MCP client can't call dispatcher.Unregister directly since Dispatcher wraps
// globalRegistry. We expose it via globalRegistry instead.

// UnregisterTool 注销一个工具（供 MCP 使用）
func UnregisterTool(name string) {
	globalRegistry.Unregister(name)
}

// GetToolDefinitions returns all registered tools as LLM-compatible definitions
func GetToolDefinitions() []map[string]interface{} {
	return globalRegistry.ToolDefinitions()
}

// RegisterDispatcherTool 注册一个工具到全局注册表（dispatcher 暴露）
func RegisterDispatcherTool(t Tool) {
	globalRegistry.Register(t)
}
