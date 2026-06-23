package kernel

import (
	"context"
	"strings"
	"testing"
)

func TestTaskRuntimeContextPromptIncludesDurableSlices(t *testing.T) {
	runtime := TaskRuntimeContext{
		TaskID:    "task_1",
		RunID:     "run_1",
		Title:     "Improve context",
		Status:    "running",
		Summary:   "Selector is being implemented.",
		NextSteps: []string{"wire gateway", "add tests"},
		Handoff: &TaskHandoffContext{
			Summary:      "Previous run added artifacts.",
			DoneItems:    []string{"created schema"},
			ChangedFiles: []string{"internal/control/store.go"},
		},
		Artifacts: []TaskArtifactContext{{
			Kind: "file",
			Name: "store.go",
			URI:  "internal/control/store.go",
		}},
		Events: []TaskEventContext{{
			Type:    "tool.completed",
			Summary: "read_file completed",
		}},
	}

	ctx := WithTaskRuntimeContext(context.Background(), runtime)
	got, ok := TaskRuntimeContextFromContext(ctx)
	if !ok {
		t.Fatal("expected runtime context")
	}
	prompt := got.Prompt(10000)
	for _, want := range []string{
		"task_1",
		"Previous run added artifacts.",
		"internal/control/store.go",
		"tool.completed",
		"read_file completed",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
