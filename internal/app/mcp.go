package app

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// initMCP connects once to each configured MCP server and registers its tools
// in every agent worker's dispatcher, so all workers share the connection and
// see the same MCP tools. Persons' read-only markings come from store; nil
// leaves only the operator's. The returned manager owns the connections and
// must be closed during shutdown.
func initMCP(cfg *config.Config, dispatchers []*tools.Dispatcher, store *control.Store) *tools.MCPToolManager {
	if len(dispatchers) == 0 || cfg == nil || len(cfg.MCP.Servers) == 0 {
		return nil
	}
	manager := tools.NewMCPToolManager(dispatchers...)
	if store != nil {
		manager.UseReadOnlyMarks(store)
	}
	for _, server := range cfg.MCP.Servers {
		resolvedName := fallbackMCPName(server.Name, server.Command, server.URL, server.Args)
		if strings.TrimSpace(server.Name) == "" {
			log.Warn("mcp: server name was generated; add an explicit stable name to config", "name", resolvedName)
		}
		mcpCfg := tools.MCPServerConfig{
			Name:      resolvedName,
			Transport: fallbackMCPTransport(server.Transport, server.Command, server.URL),
			Command:   server.Command,
			Args:      server.Args,
			URL:       server.URL,
			Headers:   server.Headers,
			Auth:      server.Auth,
			EnvFilter: server.EnvFilter,
			// The operator's exact names; annotations the server reports
			// about its own tools are never read.
			ReadOnlyTools: server.ReadOnlyTools,
		}
		if err := manager.Connect(mcpCfg); err != nil {
			log.Warn("mcp: skipped server", "name", mcpCfg.Name, "error", err)
			continue
		}
		log.Info("mcp: connected server", "name", mcpCfg.Name)
	}
	return manager
}

func fallbackMCPName(name, command, url string, args []string) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	fingerprint := sha256.Sum256([]byte(strings.TrimSpace(command) + "\x00" + strings.Join(args, "\x00") + "\x00" + strings.TrimSpace(url)))
	suffix := hex.EncodeToString(fingerprint[:4])
	if strings.TrimSpace(command) != "" {
		base := filepath.Base(command)
		return strings.TrimSuffix(base, filepath.Ext(base)) + "_" + suffix
	}
	if strings.TrimSpace(url) != "" {
		return "http_" + suffix
	}
	return "mcp_" + suffix
}

func fallbackMCPTransport(transport, command, url string) string {
	if strings.TrimSpace(transport) != "" {
		return strings.TrimSpace(transport)
	}
	if strings.TrimSpace(url) != "" {
		return "http"
	}
	if strings.TrimSpace(command) != "" {
		return "stdio"
	}
	return "stdio"
}
