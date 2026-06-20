package app

import (
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestModelSetupDiagnosticIncludesConfiguredProviderAndReason(t *testing.T) {
	cfg := &config.Config{
		Path:  "/tmp/selfmind-config.yaml",
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
	}
	cfg.Normalize()

	msg := modelSetupDiagnostic(cfg, fmt.Errorf("no credentials found for provider kimi-coding"))

	for _, want := range []string{
		"provider=kimi-coding model=kimi-for-coding",
		"Config: /tmp/selfmind-config.yaml",
		"Reason: no credentials found for provider kimi-coding",
		"selfmind model check",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic missing %q:\n%s", want, msg)
		}
	}
}
