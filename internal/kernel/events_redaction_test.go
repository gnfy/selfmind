package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
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

// Tool arguments that do not parse are refused before any tool runs, and the
// parser's message can quote them (Go names the member path of a value it
// cannot decode). The refusal passes the installed redactor on its way to the
// model, as a tool failure does.
func TestUnparseableToolArgumentRefusalUsesInstalledRedactor(t *testing.T) {
	SetAgentEventRedactor(func(value string) string {
		return strings.ReplaceAll(value, "secret-key-7d102e", "[MASKED]")
	})
	t.Cleanup(func() { SetAgentEventRedactor(nil) })
	backend := &rejectingArgumentPreparerBackend{}
	events := make(chan string, 8)
	for _, args := range []string{`{"secret-key-7d102e": 1e999}`, `{"cfg":{"secret-key-7d102e":1e999}}`} {
		result := (&Agent{backend: backend}).executeSingleToolCall(context.Background(), "default", events, 0, llm.ToolCall{
			ID: "call-unparseable", Function: "write_file", Args: args,
		})
		if result.success || backend.dispatchCalls != 0 {
			t.Fatalf("unparseable arguments were dispatched: %+v", result)
		}
		if strings.Contains(result.msg.Content, "secret-key-7d102e") || !strings.Contains(result.msg.Content, "tool_arguments_invalid") {
			t.Fatalf("the refusal shown to the model leaked its arguments: %s", result.msg.Content)
		}
	}
	for _, event := range drainAgentEvents(events) {
		if strings.Contains(event.Error+event.Content, "secret-key-7d102e") {
			t.Fatalf("the refusal event leaked its arguments: %+v", event)
		}
	}
}
