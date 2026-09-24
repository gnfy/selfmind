package tools

import (
	"context"
	"encoding/json"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/textutil"
)

// CompletedRunToolReceipts selects bounded, read-oriented output from an exact
// completed parent. It does not replay a raw transcript or grant authority.
// Missing or oversized checkpoints simply leave the existing Plan/handoff
// projection as the continuation source.
func CompletedRunToolReceipts(ctx context.Context, store *control.Store, tenantID, personID, runID string) []kernel.PriorToolReceipt {
	if store == nil {
		return nil
	}
	record, err := store.CompletedLoopCheckpointForRun(ctx, tenantID, personID, runID)
	if err != nil || record == nil || len(record.Snapshot) == 0 || len(record.Snapshot) > 4<<20 {
		return nil
	}
	var messages []llm.Message
	if json.Unmarshal(record.Snapshot, &messages) != nil {
		return nil
	}
	type call struct{ name, target string }
	calls := map[string]call{}
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, toolCall := range message.ToolCalls {
			calls[toolCall.ID] = call{name: toolCall.Function, target: receiptTarget(toolCall.Function, toolCall.Args)}
		}
	}
	seen := map[string]bool{}
	receipts := make([]kernel.PriorToolReceipt, 0, 6)
	remaining := 3600
	for i := len(messages) - 1; i >= 0 && len(receipts) < 6 && remaining > 0; i-- {
		message := messages[i]
		if message.Role != "tool" || strings.TrimSpace(message.Content) == "" {
			continue
		}
		origin := calls[message.ToolCallID]
		name := strings.TrimSpace(message.Name)
		if name == "" {
			name = origin.name
		}
		if !continuationReceiptTool(name) {
			continue
		}
		target := origin.target
		key := name + "\x00" + target
		if seen[key] {
			continue
		}
		seen[key] = true
		content := receiptContent(name, message.Content)
		content = strings.Join(strings.Fields(RedactSensitive(content)), " ")
		if content == "" {
			continue
		}
		limit := 900
		if limit > remaining {
			limit = remaining
		}
		excerpt := textutil.TruncateBytes(content, limit)
		receipts = append(receipts, kernel.PriorToolReceipt{
			Tool: name, Target: textutil.TruncateBytes(RedactSensitive(target), 120),
			Excerpt: excerpt, Truncated: len(excerpt) < len(content),
		})
		remaining -= len(excerpt)
	}
	return receipts
}

func continuationReceiptTool(name string) bool {
	switch name {
	case "read_file", "batch_read", "skill_select", "skill_view", "terminal", "verify", "tool_output_view":
		return true
	default:
		return false
	}
}

func receiptTarget(name, raw string) string {
	var args map[string]interface{}
	if json.Unmarshal([]byte(raw), &args) != nil {
		return ""
	}
	parts := []string{}
	for _, key := range []string{"path", "file", "name", "section"} {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			parts = append(parts, strings.TrimSpace(value))
		}
	}
	if len(parts) == 0 && name == "terminal" {
		if command, ok := args["command"].(string); ok {
			parts = append(parts, command)
		}
	}
	return strings.Join(parts, " ")
}

func receiptContent(name, raw string) string {
	if name != "skill_view" && name != "skill_select" {
		return raw
	}
	var value map[string]interface{}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return raw
	}
	if content, ok := value["content"].(string); ok && content != "" {
		return content
	}
	return raw
}
