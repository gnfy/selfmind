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

func TestSecretRegistryDoesNotRedactIdentifierSubstring(t *testing.T) {
	registry := NewSecretRegistry()
	registry.Register("task-orchestrator")

	input := "lid-tm-task-orchestrator-svc-configmap-release"
	if got := registry.Redact(input); got != input {
		t.Fatalf("identifier substring was corrupted: %q", got)
	}
	if got := registry.Redact("token task-orchestrator value"); got != "token [REDACTED] value" {
		t.Fatalf("standalone registered value was not redacted: %q", got)
	}
}

func TestSecretRegistryStillRedactsStructuredSecretsInsideText(t *testing.T) {
	registry := NewSecretRegistry()
	registry.Register("secret.value/with+punc")

	if got := registry.Redact("prefix=secret.value/with+punc,suffix"); got != "prefix=[REDACTED],suffix" {
		t.Fatalf("structured secret was not redacted: %q", got)
	}
}
