package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadMCPConfig(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(Options{Path: path})
}

// The operator names exact remote tools. Whether a server exposes a name is
// known only once it connects, so the loader checks only the name's shape.
func TestMCPServerReadOnlyToolsLoadAsExactNames(t *testing.T) {
	cfg, err := loadMCPConfig(t, `mcp:
  servers:
    - name: github
      transport: stdio
      command: github-mcp
      read_only_tools: [get_issue, list_pull_requests, search.code]
    - name: notion
      url: https://mcp.notion.example/mcp
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCP.Servers[0].ReadOnlyTools; !reflect.DeepEqual(got, []string{"get_issue", "list_pull_requests", "search.code"}) {
		t.Fatalf("read_only_tools = %v", got)
	}
	if len(cfg.MCP.Servers[1].ReadOnlyTools) != 0 {
		t.Fatalf("a server without read_only_tools marked %v", cfg.MCP.Servers[1].ReadOnlyTools)
	}
}

func TestMCPServerReadOnlyToolsRejectPatternsAndInexactNames(t *testing.T) {
	for name, entry := range map[string]string{
		"glob star":           `"get_*"`,
		"glob question mark":  `"get_issue?"`,
		"glob class":          `"[gs]et_issue"`,
		"empty name":          `""`,
		"surrounding space":   `" get_issue"`,
		"embedded line break": `"get\nissue"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadMCPConfig(t, "mcp:\n  servers:\n    - name: github\n      command: github-mcp\n      read_only_tools: ["+entry+"]\n")
			if err == nil || !strings.Contains(err.Error(), "mcp.servers[0].read_only_tools") {
				t.Fatalf("read_only_tools entry %s loaded: err=%v", entry, err)
			}
		})
	}
}
