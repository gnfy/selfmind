package httpapi

import (
	"selfmind/internal/kernel/llm"
	"testing"
)

func TestObserverRunIdentityComesFromCoordinator(t *testing.T) {
	var seen llm.StreamEvent
	observer := withRunStreamIdentity(func(e llm.StreamEvent) { seen = e }, "task-owned", "run-owned")
	observer(llm.StreamEvent{EventType: "tool.completed", ToolName: "finish_run", RunID: "foreign", TaskID: "foreign"})
	if seen.RunID != "run-owned" || seen.TaskID != "task-owned" || seen.ToolName != "finish_run" {
		t.Fatalf("observer identity: %+v", seen)
	}
	if withRunStreamIdentity(nil, "task", "run") != nil {
		t.Fatal("absent observer changed")
	}
}
