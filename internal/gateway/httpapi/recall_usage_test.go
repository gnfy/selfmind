package httpapi

import (
	"testing"

	"selfmind/internal/kernel"
)

func TestRecallOutputOverlapIsConservativeAndSourceAware(t *testing.T) {
	stats := recallOutputOverlap(
		"The release must verify the GitOps tag before syncing ArgoCD.",
		[]kernel.RecallSlice{
			{Source: "canonical", Ref: "mem_1", Excerpt: "GitOps releases must verify the tag before ArgoCD sync."},
			{Source: "taskcard", Ref: "task_2", Excerpt: "The user prefers short Chinese answers."},
		},
	)
	if stats.Selected != 2 || stats.Overlapping != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.OverlapSources["canonical"] != 1 || len(stats.Refs) != 1 || stats.Refs[0] != "mem_1" {
		t.Fatalf("overlap detail = %+v", stats)
	}
}

func TestRecallOutputOverlapDoesNotTreatOneGenericWordAsAdoption(t *testing.T) {
	stats := recallOutputOverlap("The task is complete.", []kernel.RecallSlice{{
		Source: "canonical", Ref: "mem_1", Excerpt: "The user prefers complete deployment reports.",
	}})
	if stats.Overlapping != 0 {
		t.Fatalf("generic overlap must not count: %+v", stats)
	}
}
