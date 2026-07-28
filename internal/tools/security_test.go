package tools

import (
	"strings"
	"testing"
)

func TestSecretRegistryRedactsOpaqueRuntimeValue(t *testing.T) {
	registry := NewSecretRegistry()
	registry.Register("opaque-value-with-no-secret-shape")

	got := registry.Redact("before opaque-value-with-no-secret-shape after")
	if strings.Contains(got, "opaque-value-with-no-secret-shape") {
		t.Fatalf("opaque secret leaked: %q", got)
	}
	if got != "before [REDACTED] after" {
		t.Fatalf("redacted value = %q", got)
	}
}

func TestSecretRegistryIgnoresShortValues(t *testing.T) {
	registry := NewSecretRegistry()
	registry.Register("yes")
	if got := registry.Redact("yes, continue"); got != "yes, continue" {
		t.Fatalf("short ordinary value must not be masked: %q", got)
	}
}
