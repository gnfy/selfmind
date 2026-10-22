// Command mcpstub is the stdio MCP server that SelfMind tests and eval
// fixtures connect to as a configured server. It is test infrastructure, not
// a product entrypoint; see package mcptest.
package main

import (
	"fmt"
	"os"

	"selfmind/internal/tools/mcptest"
)

func main() {
	if err := mcptest.Main(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mcpstub:", err)
		os.Exit(1)
	}
}
