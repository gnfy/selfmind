package httpapi

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// TestFormatTaskStatusParkedWording: an in_progress task with NO active run
// finished its turn and is only parked — the status must say so, not read as
// "still working".
func TestFormatTaskStatusParkedWording(t *testing.T) {
	task := &control.Task{Title: "build a game", Status: "in_progress"}
	card := formatTaskStatus(task, nil, nil, nil)
	if !strings.Contains(card, "turn finished — reply to continue") {
		t.Fatalf("parked in_progress task should announce the finished turn: %q", card)
	}
}

func TestInterruptedTaskSuffixUsesCompletionReason(t *testing.T) {
	got := interruptedTaskSuffix(control.LatestRunOutcome{CompletionReason: "daemon_recovery", Resumable: true})
	if got != "daemon restarted - resumable" {
		t.Fatalf("suffix = %q", got)
	}
	got = interruptedTaskSuffix(control.LatestRunOutcome{CompletionReason: "provider_or_transport_error", Resumable: true})
	if got != "provider connection interrupted - resumable" {
		t.Fatalf("provider suffix = %q", got)
	}
}

func TestVerificationPartialStatusIsExplicitAndResumable(t *testing.T) {
	task := &control.Task{ID: "task_verify", Title: "change code", Status: api.RunStatusVerificationPartial}
	card := formatTaskStatus(task, nil, nil, nil)
	if !strings.Contains(card, "work changed; verification incomplete") {
		t.Fatalf("status card=%q", card)
	}
}

// TestFormatTaskStatusRunningShowsElapsed: a task with a live run still reports
// elapsed and does NOT get the parked wording.
func TestFormatTaskStatusRunningShowsElapsed(t *testing.T) {
	task := &control.Task{Title: "build a game", Status: "in_progress"}
	active := &activeRun{StartedAt: time.Now().Add(-5 * time.Second)}
	card := formatTaskStatus(task, nil, active, nil)
	if strings.Contains(card, "turn finished") {
		t.Fatalf("a running task must not show the parked wording: %q", card)
	}
	if !strings.Contains(card, "elapsed") {
		t.Fatalf("a running task must still show elapsed: %q", card)
	}
}

func TestFormatTaskStatusUsesUnambiguousPlanMarkers(t *testing.T) {
	task := &control.Task{Title: "ship release", Status: "in_progress"}
	plan := []taskPlanStep{
		{Step: "inspect", Status: "completed"},
		{Step: "deploy", Status: "in_progress"},
		{Step: "verify", Status: "pending"},
		{Step: "optional cleanup", Status: "cancelled"},
	}
	card := formatTaskStatus(task, nil, nil, plan)
	for _, want := range []string{"- ✓ inspect", "- → deploy", "- ○ verify", "- − optional cleanup"} {
		if !strings.Contains(card, want) {
			t.Fatalf("status card missing %q: %q", want, card)
		}
	}
	if strings.Contains(card, "[x]") {
		t.Fatalf("status card must not use an ambiguous x marker: %q", card)
	}
}
