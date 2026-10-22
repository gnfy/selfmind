package kernel

import (
	"fmt"

	"selfmind/internal/kernel/llm"
)

// toolCallIDs reserves protocol identities for the whole loop, including the
// restored ledger. A continuation restarts its iteration counter, and context
// compaction can remove older pairs, so neither is an identity boundary.
type toolCallIDs struct {
	prefix string
	used   map[string]bool
}

func newToolCallIDs(prefix string, messages []llm.Message) *toolCallIDs {
	ids := &toolCallIDs{prefix: prefix, used: make(map[string]bool)}
	ids.reserveMessages(messages)
	return ids
}

func (ids *toolCallIDs) reserveMessages(messages []llm.Message) {
	for _, message := range messages {
		if message.ToolCallID != "" {
			ids.used[message.ToolCallID] = true
		}
		for _, call := range message.ToolCalls {
			if call.ID != "" {
				ids.used[call.ID] = true
			}
		}
	}
}

func (ids *toolCallIDs) allocate(base string) string {
	id := ids.prefix + base
	for suffix := 1; ids.used[id]; suffix++ {
		id = fmt.Sprintf("%s%s-%d", ids.prefix, base, suffix)
	}
	ids.used[id] = true
	return id
}

func (ids *toolCallIDs) normalize(calls []llm.ToolCall, iteration int) []llm.ToolCall {
	out := append([]llm.ToolCall(nil), calls...)
	// Explicit provider identities stay paired verbatim. Reserve the whole
	// batch first so a missing identity cannot shadow a later explicit one.
	for _, call := range out {
		if call.ID != "" {
			ids.used[call.ID] = true
		}
	}
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = ids.allocate(fmt.Sprintf("toolcall-%d-%d", iteration, i))
		}
	}
	return out
}

func (ids *toolCallIDs) legacy(calls []ToolCall, iteration int) []llm.ToolCall {
	out := make([]llm.ToolCall, 0, len(calls))
	for i, call := range calls {
		out = append(out, llm.ToolCall{
			ID:       ids.allocate(fmt.Sprintf("legacy-toolcall-%d-%d", iteration, i)),
			Function: call.Name, Args: call.Args,
		})
	}
	return out
}
