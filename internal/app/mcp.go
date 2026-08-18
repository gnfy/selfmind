package app

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// InitMCP connects to all configured MCP servers, registers their tools, and
// returns the lifecycle owner that callers must close during shutdown.
func InitMCP(disp *tools.Dispatcher, cfg *config.Config) *tools.MCPToolManager {
	if disp == nil || cfg == nil || len(cfg.MCP.Servers) == 0 {
		return nil
	}
	manager := tools.NewMCPToolManager(disp)
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
