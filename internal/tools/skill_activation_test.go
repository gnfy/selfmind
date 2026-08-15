package tools

import (
	"context"
	"testing"

	"selfmind/internal/kernel"
)

func TestLegacySkillToolEmitsVersionedActivation(t *testing.T) {
	events := make(chan string, 1)
	ctx := kernel.WithEventChannel(context.Background(), events)
	tool := &SkillTool{
		BaseTool:    BaseTool{name: "skill:inspect"},
		source:      "agent-created",
		versionHash: "version-1",
	}

	if _, err := tool.Execute(map[string]interface{}{"_context": ctx}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	select {
	case raw := <-events:
		event, ok := kernel.DecodeAgentEvent(raw)
		if !ok || event.Type != "skill.activated" {
			t.Fatalf("event = %#v ok=%v", event, ok)
		}
		if event.Payload["name"] != "inspect" || event.Payload["version_hash"] != "version-1" {
			t.Fatalf("payload = %#v", event.Payload)
		}
	default:
		t.Fatal("skill activation event was not emitted")
	}
}
