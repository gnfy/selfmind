package kernel

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRuntimeObservationIsFrozenAndHistoryKeepsProvenance(t *testing.T) {
	observation := &RuntimeObservation{ObservedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BuildFingerprint: "current-build", MaxActiveWorkRuns: 2}
	runtime := TaskRuntimeContext{RunID: "current-run", RuntimeObservation: observation}
	first, second := runtime.Prompt(8000), runtime.Prompt(8000)
	if first != second || !strings.Contains(first, "running_build_fingerprint: current-build") || !strings.Contains(first, "initial_continuation: none") {
		t.Fatalf("runtime observation was absent or unstable: %s", first)
	}
	observation.ResumesRunID = "exact-parent"
	if next := runtime.Prompt(8000); !strings.Contains(next, "initial_continuation_parent: exact-parent") || strings.Contains(next, "initial_continuation: none") {
		t.Fatalf("explicit continuation evidence missing: %s", next)
	}
	old := spineEntry{Kind: spineEntryKind, User: "Inspect the current runtime", Assistant: "It runs old-build; attached to an earlier run."}
	message := old.toMessages()[0]
	if !strings.Contains(message.Content, "unavailable (legacy historical record)") || !strings.Contains(message.Content, "not current execution or state evidence") || len(message.ToolCalls) != 0 {
		t.Fatalf("history acquired execution authority: %+v", message)
	}
	saved := buildSpineEntry(context.Background(), "latest request", "verified response", "completed", nil)
	if _, err := time.Parse(time.RFC3339, saved.ObservedAt); err != nil {
		t.Fatalf("new reference lost observation time: %+v", saved)
	}
}
