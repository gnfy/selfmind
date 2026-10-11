package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"selfmind/internal/platform/log"
)

// MCPToolManager owns MCP client lifetimes and keeps every attached
// Dispatcher registry in sync with server catalogue changes. Agent workers
// share one manager: each configured server has one connection, and every
// worker's dispatcher registers the same tools bound to that connection.
type MCPToolManager struct {
	mu          sync.RWMutex
	clients     map[string]*MCPClient
	registered  map[string]map[string]struct{}
	toolOwners  map[string]string
	seenConfigs map[string]struct{}
	configured  int
	failures    map[string]string
	dispatchers []*Dispatcher
	// readOnly holds every configured server's read-only markings, including
	// a server whose connection failed, so its markings stay listable.
	readOnly map[string]*mcpReadOnlyPolicy
	marks    MCPReadOnlyMarks
	// warnings are per-server diagnostics that do not fail the server.
	warnings map[string]string
}

type MCPServerFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// MCPServerWarning is a configuration problem that leaves the server usable,
// such as an operator read-only name the server does not expose.
type MCPServerWarning struct {
	Name    string `json:"name"`
	Warning string `json:"warning"`
}

type MCPHealthSnapshot struct {
	Configured int                `json:"configured"`
	Connected  int                `json:"connected"`
	Failed     int                `json:"failed"`
	Failures   []MCPServerFailure `json:"failures,omitempty"`
	Warnings   []MCPServerWarning `json:"warnings,omitempty"`
}

// MCPServerStatus describes one configured server for read-only marking: the
// identity markings are bound to, whether it is connected, the remote tool
// names it exposes now, and the operator's read-only names.
type MCPServerStatus struct {
	Name             string
	Identity         string
	Connected        bool
	Tools            []string
	OperatorReadOnly []string
}

// NewMCPToolManager registers MCP tools into every given dispatcher. Pass all
// agent workers' dispatchers to one manager; a manager per dispatcher would
// start one server process per worker.
func NewMCPToolManager(dispatchers ...*Dispatcher) *MCPToolManager {
	return &MCPToolManager{
		clients:     make(map[string]*MCPClient),
		registered:  make(map[string]map[string]struct{}),
		toolOwners:  make(map[string]string),
		seenConfigs: make(map[string]struct{}),
		failures:    make(map[string]string),
		dispatchers: append([]*Dispatcher(nil), dispatchers...),
		readOnly:    make(map[string]*mcpReadOnlyPolicy),
		warnings:    make(map[string]string),
	}
}

// UseReadOnlyMarks supplies the persons' read-only markings. Call it before
// Connect: a server keeps the source it was connected with.
func (m *MCPToolManager) UseReadOnlyMarks(marks MCPReadOnlyMarks) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marks = marks
}

func (m *MCPToolManager) Connect(config MCPServerConfig) error {
	m.mu.Lock()
	m.configured++
	_, exists := m.seenConfigs[config.Name]
	if !exists {
		m.seenConfigs[config.Name] = struct{}{}
		m.readOnly[config.Name] = newMCPReadOnlyPolicy(config, m.marks)
	}
	m.mu.Unlock()
	if exists {
		err := fmt.Errorf("server name %s is configured more than once", config.Name)
		m.recordFailure(config.Name, err)
		return err
	}

	client, err := newMCPClient(config, func(changedClient *MCPClient, tools map[string]MCPToolDef) {
		m.syncTools(config.Name, changedClient, tools)
	})
	if err != nil {
		err = fmt.Errorf("connect %s: %w", config.Name, err)
		m.recordFailure(config.Name, err)
		return err
	}

	m.mu.Lock()
	if _, exists := m.clients[config.Name]; exists {
		m.mu.Unlock()
		_ = client.Close()
		err := fmt.Errorf("server %s already connected", config.Name)
		m.recordFailure(config.Name, err)
		return err
	}
	m.clients[config.Name] = client
	m.registered[config.Name] = make(map[string]struct{})
	delete(m.failures, config.Name)
	m.mu.Unlock()
	m.syncTools(config.Name, client, client.GetTools())
	return nil
}

func (m *MCPToolManager) syncTools(serverName string, client *MCPClient, defs map[string]MCPToolDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clients[serverName] != client {
		return
	}
	previous := m.registered[serverName]
	next := make(map[string]struct{}, len(defs))
	var syncErr error
	for remoteName, def := range defs {
		localName := MCPToolLocalName(serverName, remoteName)
		if owner, exists := m.toolOwners[localName]; exists && owner != serverName {
			syncErr = errors.Join(syncErr, fmt.Errorf("local tool name %s is already owned by MCP server %s", localName, owner))
			continue
		}
		// A collision in any one dispatcher keeps the tool out of all of
		// them, so every worker keeps the same catalogue.
		if m.toolOwners[localName] == "" && m.registeredAnywhereLocked(localName) {
			syncErr = errors.Join(syncErr, fmt.Errorf("local tool name %s collides with an existing registered tool", localName))
			continue
		}
		next[localName] = struct{}{}
		m.toolOwners[localName] = serverName
		tool := client.WrapToolNamed(def, localName)
		tool.readOnly = m.readOnly[serverName]
		for _, dispatcher := range m.dispatchers {
			dispatcher.RegisterTool(tool)
		}
	}
	for localName := range previous {
		if _, exists := next[localName]; !exists {
			if m.toolOwners[localName] == serverName {
				m.unregisterEverywhere(localName)
				delete(m.toolOwners, localName)
			}
		}
	}
	m.registered[serverName] = next
	if syncErr != nil {
		m.recordFailureLocked(serverName, syncErr)
	} else {
		delete(m.failures, serverName)
	}
	m.recordReadOnlyWarningLocked(serverName, defs)
}

// recordReadOnlyWarningLocked reports operator read-only names the server's
// current catalogue does not expose. They mark nothing, and the server stays
// connected: a configuration typo is a diagnostic, not an outage.
func (m *MCPToolManager) recordReadOnlyWarningLocked(serverName string, defs map[string]MCPToolDef) {
	var missing []string
	if policy := m.readOnly[serverName]; policy != nil {
		for name := range policy.operator {
			if _, exposed := defs[name]; !exposed {
				missing = append(missing, name)
			}
		}
	}
	sort.Strings(missing)
	warning := ""
	if len(missing) > 0 {
		warning = "read_only_tools names tools this server does not expose: " + strings.Join(missing, ", ")
	}
	if warning == m.warnings[serverName] {
		return
	}
	if warning == "" {
		delete(m.warnings, serverName)
		return
	}
	m.warnings[serverName] = warning
	log.Warn("mcp: read_only_tools names tools the server does not expose; they mark nothing", "name", serverName, "tools", strings.Join(missing, ", "))
}

// Disconnect removes the server's tools from every dispatcher under the
// catalogue lock, then closes its connection outside it: a closing session
// may still deliver a catalogue change, which takes the lock.
func (m *MCPToolManager) Disconnect(name string) error {
	m.mu.Lock()
	client, ok := m.clients[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("server %s not found", name)
	}
	registered := m.registered[name]
	delete(m.clients, name)
	delete(m.registered, name)
	delete(m.warnings, name)
	for localName := range registered {
		m.unregisterEverywhere(localName)
		if m.toolOwners[localName] == name {
			delete(m.toolOwners, localName)
		}
	}
	m.mu.Unlock()
	return client.Close()
}

// Close removes every server's tools as Disconnect does, then closes the
// connections.
func (m *MCPToolManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	clients := m.clients
	for _, names := range m.registered {
		for localName := range names {
			m.unregisterEverywhere(localName)
		}
	}
	m.clients = make(map[string]*MCPClient)
	m.registered = make(map[string]map[string]struct{})
	m.toolOwners = make(map[string]string)
	m.mu.Unlock()

	var errs []error
	for name, client := range clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close MCP server %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *MCPToolManager) registeredAnywhereLocked(localName string) bool {
	for _, dispatcher := range m.dispatchers {
		if _, exists := dispatcher.registry.Get(localName); exists {
			return true
		}
	}
	return false
}

func (m *MCPToolManager) unregisterEverywhere(localName string) {
	for _, dispatcher := range m.dispatchers {
		dispatcher.UnregisterTool(localName)
	}
}

// HoldCatalog runs fn while no MCP catalogue change can be applied: every
// registration and removal, including Disconnect and Close, happens under
// the same lock. fn therefore sees the same MCP tools in every dispatcher.
// fn must not call the manager.
func (m *MCPToolManager) HoldCatalog(fn func()) {
	if m == nil {
		fn()
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	fn()
}

func (m *MCPToolManager) recordFailure(name string, err error) {
	if m == nil || err == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordFailureLocked(name, err)
}

func (m *MCPToolManager) recordFailureLocked(name string, err error) {
	message := strings.Join(strings.Fields(RedactSensitive(err.Error())), " ")
	m.failures[strings.TrimSpace(name)] = truncateRunes(message, 300)
}

func (m *MCPToolManager) Health() MCPHealthSnapshot {
	if m == nil {
		return MCPHealthSnapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.failures))
	for name := range m.failures {
		names = append(names, name)
	}
	sort.Strings(names)
	failures := make([]MCPServerFailure, 0, len(names))
	for _, name := range names {
		failures = append(failures, MCPServerFailure{Name: name, Error: m.failures[name]})
	}
	warned := make([]string, 0, len(m.warnings))
	for name := range m.warnings {
		warned = append(warned, name)
	}
	sort.Strings(warned)
	var warnings []MCPServerWarning
	for _, name := range warned {
		warnings = append(warnings, MCPServerWarning{Name: name, Warning: m.warnings[name]})
	}
	return MCPHealthSnapshot{
		Configured: m.configured,
		Connected:  len(m.clients),
		Failed:     len(failures),
		Failures:   failures,
		Warnings:   warnings,
	}
}

// Servers lists every configured server, connected or not, in name order.
func (m *MCPToolManager) Servers() []MCPServerStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	out := make([]MCPServerStatus, 0, len(m.readOnly))
	clients := make([]*MCPClient, 0, len(m.readOnly))
	for name, policy := range m.readOnly {
		status := MCPServerStatus{Name: name, Identity: policy.identity}
		for tool := range policy.operator {
			status.OperatorReadOnly = append(status.OperatorReadOnly, tool)
		}
		sort.Strings(status.OperatorReadOnly)
		client := m.clients[name]
		status.Connected = client != nil
		out = append(out, status)
		clients = append(clients, client)
	}
	m.mu.RUnlock()
	for index, client := range clients {
		if client == nil {
			continue
		}
		for tool := range client.GetTools() {
			out[index].Tools = append(out[index].Tools, tool)
		}
		sort.Strings(out[index].Tools)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *MCPToolManager) ListServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *MCPToolManager) ListTools(serverName string) []MCPToolDef {
	m.mu.RLock()
	client := m.clients[serverName]
	m.mu.RUnlock()
	if client == nil {
		return nil
	}
	tools := client.GetTools()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]MCPToolDef, 0, len(names))
	for _, name := range names {
		result = append(result, tools[name])
	}
	return result
}

// filterEnv applies an optional MCP-specific allow-list. With no allow-list,
// BuildProcessEnv receives the operator's ordinary tool environment and strips
// SelfMind control-plane state according to the shared process policy.
func filterEnv(whitelist []string) []string {
	if len(whitelist) == 0 {
		return os.Environ()
	}
	allowed := make(map[string]struct{}, len(whitelist))
	for _, name := range whitelist {
		allowed[name] = struct{}{}
	}
	result := make([]string, 0, len(allowed))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, ok := allowed[name]; ok {
			result = append(result, entry)
		}
	}
	return result
}

func MCPToolLocalName(serverName, toolName string) string {
	name := "mcp_" + sanitizeMCPName(serverName) + "_" + sanitizeMCPName(toolName)
	const maxToolName = 64
	digest := sha256.Sum256([]byte(serverName + "\x00" + toolName))
	suffix := "_" + hex.EncodeToString(digest[:4])
	if len(name)+len(suffix) > maxToolName {
		name = strings.TrimRight(name[:maxToolName-len(suffix)], "_")
	}
	return name + suffix
}

func sanitizeMCPName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	if result := strings.Trim(b.String(), "_"); result != "" {
		return result
	}
	return "tool"
}
