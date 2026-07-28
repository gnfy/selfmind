package kernel

import (
	"strings"
	"testing"
)

func TestEncodeAgentEventUsesInstalledRedactor(t *testing.T) {
	SetAgentEventRedactor(func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[MASKED]")
	})
	t.Cleanup(func() { SetAgentEventRedactor(nil) })
	encoded := EncodeAgentEvent(AgentEvent{
		Type:       "tool.started",
		ToolArgs:   `{"token":"secret-value"}`,
		ToolResult: "secret-value",
		Payload:    map[string]interface{}{"command": "echo secret-value"},
	})
	if strings.Contains(encoded, "secret-value") {
		t.Fatalf("event leaked secret: %s", encoded)
	}
}
