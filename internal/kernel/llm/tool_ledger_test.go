package llm

import "testing"

func TestSanitizeToolMessageLedgerDropsOrphanedToolResult(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "read"},
		{Role: "tool", ToolCallID: "missing", Content: "result"},
		{Role: "assistant", Content: "done"},
	}

	got := sanitizeToolMessageLedger(messages)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("unexpected messages: %+v", got)
	}
}

func TestSanitizeToolMessageLedgerDropsAssistantCallWithoutResult(t *testing.T) {
	messages := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Function: "read_file"}}},
		{Role: "user", Content: "continue"},
	}

	got := sanitizeToolMessageLedger(messages)
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", got)
	}
}

func TestSanitizeToolMessageLedgerKeepsMatchedToolPair(t *testing.T) {
	messages := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Function: "read_file"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "ok"},
	}

	got := sanitizeToolMessageLedger(messages)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool call not preserved: %+v", got[0])
	}
	if got[1].ToolCallID != "call_1" {
		t.Fatalf("tool result not preserved: %+v", got[1])
	}
	if len(messages[0].ToolCalls) != 1 {
		t.Fatalf("source messages mutated: %+v", messages[0])
	}
}
