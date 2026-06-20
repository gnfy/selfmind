package app

import (
	"path/filepath"
	"strings"

	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// InitMCP connects to all configured MCP servers and registers their tools.
func InitMCP(disp *tools.Dispatcher, cfg *config.Config) {
	if disp == nil || cfg == nil || len(cfg.MCP.Servers) == 0 {
		return
	}
	manager := tools.NewMCPToolManager(disp)
	for _, server := range cfg.MCP.Servers {
		mcpCfg := tools.MCPServerConfig{
			Name:      fallbackMCPName(server.Name, server.Command, server.URL),
			Transport: fallbackMCPTransport(server.Transport, server.Command, server.URL),
			Command:   server.Command,
			Args:      server.Args,
			URL:       server.URL,
			Headers:   server.Headers,
			Auth:      server.Auth,
			EnvFilter: server.EnvFilter,
		}
		if err := manager.Connect(mcpCfg); err != nil {
			log.Warn("mcp: skipped server", "name", mcpCfg.Name, "error", err)
			continue
		}
		log.Info("mcp: connected server", "name", mcpCfg.Name)
	}
}

func fallbackMCPName(name, command, url string) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	if strings.TrimSpace(command) != "" {
		base := filepath.Base(command)
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	if strings.TrimSpace(url) != "" {
		return "http"
	}
	return "mcp"
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
