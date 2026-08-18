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
)

// MCPToolManager owns MCP client lifetimes and keeps the Dispatcher registry in
// sync with server catalogue changes.
type MCPToolManager struct {
	mu          sync.RWMutex
	clients     map[string]*MCPClient
	registered  map[string]map[string]struct{}
	toolOwners  map[string]string
	seenConfigs map[string]struct{}
	configured  int
	failures    map[string]string
	dispatcher  *Dispatcher
}

type MCPServerFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type MCPHealthSnapshot struct {
	Configured int                `json:"configured"`
	Connected  int                `json:"connected"`
	Failed     int                `json:"failed"`
	Failures   []MCPServerFailure `json:"failures,omitempty"`
}

func NewMCPToolManager(dispatcher *Dispatcher) *MCPToolManager {
	return &MCPToolManager{
		clients:     make(map[string]*MCPClient),
		registered:  make(map[string]map[string]struct{}),
		toolOwners:  make(map[string]string),
		seenConfigs: make(map[string]struct{}),
		failures:    make(map[string]string),
		dispatcher:  dispatcher,
	}
}

func (m *MCPToolManager) Connect(config MCPServerConfig) error {
	m.mu.Lock()
	m.configured++
	_, exists := m.seenConfigs[config.Name]
	if !exists {
		m.seenConfigs[config.Name] = struct{}{}
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
		if _, exists := m.dispatcher.registry.Get(localName); exists && m.toolOwners[localName] == "" {
			syncErr = errors.Join(syncErr, fmt.Errorf("local tool name %s collides with an existing registered tool", localName))
			continue
		}
		next[localName] = struct{}{}
		m.toolOwners[localName] = serverName
		m.dispatcher.RegisterTool(client.WrapToolNamed(def, localName))
	}
	for localName := range previous {
		if _, exists := next[localName]; !exists {
			if m.toolOwners[localName] == serverName {
				m.dispatcher.UnregisterTool(localName)
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
}

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
	for localName := range registered {
		if m.toolOwners[localName] == name {
			delete(m.toolOwners, localName)
		}
	}
	m.mu.Unlock()

	for localName := range registered {
		m.dispatcher.UnregisterTool(localName)
	}
	return client.Close()
}

func (m *MCPToolManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	clients := m.clients
	registered := m.registered
	m.clients = make(map[string]*MCPClient)
	m.registered = make(map[string]map[string]struct{})
	m.toolOwners = make(map[string]string)
	m.mu.Unlock()

	for _, names := range registered {
		for localName := range names {
			m.dispatcher.UnregisterTool(localName)
		}
	}
	var errs []error
	for name, client := range clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close MCP server %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
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
	return MCPHealthSnapshot{
		Configured: m.configured,
		Connected:  len(m.clients),
		Failed:     len(failures),
		Failures:   failures,
	}
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
