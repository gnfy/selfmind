package app

import "testing"

func TestFallbackMCPNameIsStableAndEndpointSpecific(t *testing.T) {
	first := fallbackMCPName("", "", "https://one.example/mcp", nil)
	again := fallbackMCPName("", "", "https://one.example/mcp", nil)
	second := fallbackMCPName("", "", "https://two.example/mcp", nil)
	if first == "" || first != again {
		t.Fatalf("generated name is not stable: first=%q again=%q", first, again)
	}
	if first == second {
		t.Fatalf("different unnamed endpoints collided: %q", first)
	}
	if got := fallbackMCPName(" explicit ", "ignored", "ignored", nil); got != "explicit" {
		t.Fatalf("explicit name = %q", got)
	}
}

func TestFallbackMCPNameIncludesStdioArguments(t *testing.T) {
	a := fallbackMCPName("", "npx", "", []string{"server-a"})
	b := fallbackMCPName("", "npx", "", []string{"server-b"})
	if a == b {
		t.Fatalf("different stdio commands collided: %q", a)
	}
}
