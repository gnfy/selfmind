package kernel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Main judges whether an old check still suffices, so it must see how old the
// check is without dating a timestamp itself. The age comes from the selector,
// which keeps the rendered prompt identical for every call of the Run.
func TestPriorStepEvidenceShowsSelectedAge(t *testing.T) {
	checkedAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	runtime := TaskRuntimeContext{InheritedEvidence: []InheritedEvidenceItem{
		{StepID: "step_fresh", SourceStatus: "completed", PriorVerification: "passed", Target: "value.txt", CheckedAt: checkedAt, Age: 3*time.Minute + 20*time.Second},
		{StepID: "step_old", SourceStatus: "completed", PriorVerification: "passed", Target: "deploy", CheckedAt: checkedAt, Age: 50 * time.Hour},
		{StepID: "step_undated", SourceStatus: "completed", PriorVerification: "passed", Target: "other"},
	}}
	prompt := runtime.Prompt(10000)
	for _, want := range []string{"checked_ago=3m", "checked_ago=2d2h", "checked_at=2026-09-23T10:00:00Z"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if prompt != runtime.Prompt(10000) {
		t.Fatal("rendering must not depend on the wall clock")
	}
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{20 * time.Second, "under 1m"},
		{59 * time.Minute, "59m"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
		{47*time.Hour + 59*time.Minute, "47h59m"},
		{4*24*time.Hour + 2*time.Hour, "4d2h"},
	} {
		if got := EvidenceAge(tc.age); got != tc.want {
			t.Errorf("EvidenceAge(%v)=%q, want %q", tc.age, got, tc.want)
		}
	}
}

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
