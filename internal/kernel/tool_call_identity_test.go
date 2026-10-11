package kernel

import (
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestGeneratedToolCallIDsReserveRestoredAndExplicitIdentities(t *testing.T) {
	for _, prefix := range []string{"", "sub-child-"} {
		t.Run(prefix, func(t *testing.T) {
			messages := []llm.Message{
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: prefix + "toolcall-0-0"}, {ID: prefix + "legacy-toolcall-0-0"}}},
				{Role: "tool", ToolCallID: prefix + "toolcall-0-0-1"},
			}
			ids := newToolCallIDs(prefix, messages)
			calls := []llm.ToolCall{{Function: "terminal"}, {ID: prefix + "toolcall-0-0-2", Function: "write_file"}}
			normalized := ids.normalize(calls, 0)
			if normalized[0].ID != prefix+"toolcall-0-0-3" || normalized[1].ID != calls[1].ID || calls[0].ID != "" {
				t.Fatalf("restored/explicit identity shadowed or input mutated: %+v", normalized)
			}
			// The loop keeps the reservations even after the request history is
			// compacted. Reusing a local iteration must still create a new call.
			if again := ids.normalize(calls[:1], 0); again[0].ID == normalized[0].ID {
				t.Fatal("a later model response reused a generated identity")
			}
			legacy := ids.legacy([]ToolCall{{Name: "terminal", Args: `{}`}}, 0)
			if legacy[0].ID != prefix+"legacy-toolcall-0-0-1" {
				t.Fatalf("legacy continuation reused restored identity: %+v", legacy)
			}
			// Native identity collisions must reach the existing protocol/ledger
			// checks; normalization cannot invent authority by renaming them.
			explicit := ids.normalize([]llm.ToolCall{{ID: messages[0].ToolCalls[0].ID}}, 0)
			if explicit[0].ID != messages[0].ToolCalls[0].ID {
				t.Fatal("explicit provider identity was rewritten")
			}
		})
	}
}
