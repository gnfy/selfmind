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
// parser's message can quote them: every Go release quotes the offending number,
// and the releases whose encoding/json is built on json/v2 (1.27 and later) also
// name the member path. The refusal passes the installed redactor on its way to
// the model, as a tool failure does, so the redactor masks both and the test
// holds whichever the toolchain quotes.
func TestUnparseableToolArgumentRefusalUsesInstalledRedactor(t *testing.T) {
	SetAgentEventRedactor(func(value string) string {
		return strings.NewReplacer("secret-key-7d102e", "[MASKED]", "1e999", "[MASKED]").Replace(value)
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
		if strings.Contains(result.msg.Content, "secret-key-7d102e") || strings.Contains(result.msg.Content, "1e999") || result.errorCode != "tool_arguments_invalid" || !strings.Contains(result.msg.Content, "[MASKED]") {
			t.Fatalf("the refusal shown to the model leaked its arguments: %s", result.msg.Content)
		}
	}
	for _, event := range drainAgentEvents(events) {
		if text := event.Error + event.Content; strings.Contains(text, "secret-key-7d102e") || strings.Contains(text, "1e999") {
			t.Fatalf("the refusal event leaked its arguments: %+v", event)
		}
	}
}
