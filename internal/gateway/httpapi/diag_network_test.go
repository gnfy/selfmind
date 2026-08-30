package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestProviderNetworkDiagGivesConcreteSystemProxyRecovery(t *testing.T) {
	var output strings.Builder
	writeProviderNetworkDiag(&output, llm.ProviderNetworkStatus{
		Mode: "proxy", Source: "system", Endpoint: "127.0.0.1:7897", Reachability: "unreachable",
	})
	got := output.String()
	for _, want := range []string{"proxy via system", "127.0.0.1:7897", "start Clash", "disable its macOS System Proxy", "detected automatically"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic %q missing %q", got, want)
		}
	}
}

func TestProviderNetworkDiagGivesConcreteEnvironmentProxyRecovery(t *testing.T) {
	var output strings.Builder
	writeProviderNetworkDiag(&output, llm.ProviderNetworkStatus{
		Mode: "proxy", Source: "environment", Endpoint: "127.0.0.1:7897", Reachability: "unreachable",
	})
	got := output.String()
	for _, want := range []string{"start the configured proxy", "selfmind env refresh --restart"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic %q missing %q", got, want)
		}
	}
}
