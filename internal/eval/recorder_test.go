package eval

import (
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

func TestRecorderCountsVisibleAssistantProgressBeforeTool(t *testing.T) {
	recorder, err := NewRecorder(filepath.Join(t.TempDir(), "run.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	recorder.StartCase(&Case{ID: "progress", Title: "progress"}, "test", "test", t.TempDir())
	recorder.StartTurn(0, "inspect", "cli")
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "stream", Content: "Inspecting the workspace first."})
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "tool.started", ToolName: "read_file"})
	// A second tool in the same tool batch must not count the same prose twice.
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "tool.started", ToolName: "search_files"})
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "stream", Content: "Final answer without another tool."})
	recorder.FinishTurn(0, 200, "done", "", 1, 1, time.Now())

	if got := recorder.Snapshot().ProgressUpdates; got != 1 {
		t.Fatalf("progress updates=%d, want 1", got)
	}
}
