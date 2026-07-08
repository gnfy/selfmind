package llm

import "strings"

// sanitizeToolMessageLedger removes broken historical native-tool pairs before
// an adapter serializes the request. Context compaction/trimming can otherwise
// leave a tool result without its assistant tool_call, or a tool_call without
// its result; strict providers reject both shapes.
func sanitizeToolMessageLedger(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	toolResults := map[string]int{}
	for _, msg := range messages {
		if msg.Role != "tool" {
			continue
		}
		if id := strings.TrimSpace(msg.ToolCallID); id != "" {
			toolResults[id]++
		}
	}

	pending := map[string]int{}
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			kept := msg
			kept.ToolCalls = nil
			for _, call := range msg.ToolCalls {
				id := strings.TrimSpace(call.ID)
				if id == "" || toolResults[id] <= 0 {
					continue
				}
				kept.ToolCalls = append(kept.ToolCalls, call)
				pending[id]++
			}
			if len(kept.ToolCalls) == 0 && strings.TrimSpace(kept.Content) == "" {
				continue
			}
			out = append(out, kept)
			continue
		}
		if msg.Role == "tool" {
			id := strings.TrimSpace(msg.ToolCallID)
			if id == "" || pending[id] <= 0 {
				continue
			}
			pending[id]--
			out = append(out, msg)
			continue
		}
		out = append(out, msg)
	}
	return out
}
