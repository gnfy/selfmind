package app

import (
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// InitMCP connects to all configured MCP servers and registers their tools.
func InitMCP(disp *tools.Dispatcher, cfg *config.Config) {
	// TODO: implement MCP server connections from cfg.MCP
}
