package eval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

// Recorded turns and tool events are evidence that outlives the run. Every
// preview, the input and output ones included, is masked, and masked before
// it is cut, so a cut cannot leave the start of a credential that is no
// longer recognizable as one.
func TestRecorderMasksEveryPreviewBeforeCuttingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	recorder, err := NewRecorder(path, true)
	if err != nil {
		t.Fatal(err)
	}
	recorder.StartCase(&Case{ID: "redaction", Title: "redaction"}, "test", "test", t.TempDir())
	const token = "ghp_R7k2Qx9LmN4pT8vW1yZ3aB6cD0eF5gH2jK7s"
	recorder.StartTurn(0, "use API_TOKEN=r13input7d102e please", "cli")
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "tool.started", ToolName: "terminal", ToolArgs: strings.Repeat("a", 290) + " " + token})
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", ToolResult: strings.Repeat("r", 490) + " " + token})
	recorder.ObserveStreamEvent(llm.StreamEvent{EventType: "tool.output", ToolName: "terminal", Content: strings.Repeat("o", 490) + " " + token})
	recorder.FinishTurn(0, 200, strings.Repeat("f", 390)+" "+token+" and password=r13output7d102e", "", 1, 1, time.Now())
	recorder.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"R7k2Q", "r13input7d102e", "r13output7d102e"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("the recording leaked %q: %s", leaked, data)
		}
	}
}

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

func TestRecorderSeparatesProvenNonDispatchFromFailedOrUnknownAttempts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
		want    int
	}{
		{"held", map[string]interface{}{"invoked": false, "effect_state": "not_dispatched", "error_category": "policy_redirect"}, 0},
		{"invalid arguments", map[string]interface{}{"invoked": false, "effect_state": "not_dispatched", "error_category": "invalid_input"}, 0},
		{"attempted", map[string]interface{}{"invoked": true, "effect_state": "not_dispatched"}, 1},
		{"unknown effect", map[string]interface{}{"invoked": false, "effect_state": "unknown"}, 1},
		{"historical missing facts", nil, 1},
		{"untyped false", map[string]interface{}{"invoked": "false", "effect_state": "not_dispatched"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRecorder(filepath.Join(t.TempDir(), "run.jsonl"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.StartCase(&Case{ID: "facts"}, "test", "test", t.TempDir())
			r.ObserveStreamEvent(llm.StreamEvent{EventType: "tool.completed", ToolName: "read_file", Err: errors.New("fixture"), Payload: tc.payload})
			snap := r.Snapshot()
			if snap.ToolErrors != 1 || snap.InvokedToolErrors != tc.want {
				t.Fatalf("snapshot=%+v", snap)
			}
			r.StartCase(&Case{ID: "next"}, "test", "test", t.TempDir())
			if r.Snapshot().InvokedToolErrors != 0 {
				t.Fatal("counter leaked across cases")
			}
		})
	}
}
