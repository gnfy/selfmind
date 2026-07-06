package httpapi

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
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

// TestFormatTasksMarksParked: /tasks marks a parked (in_progress, not the active
// task) task as paused while leaving the active task and terminal tasks plain.
func TestFormatTasksMarksParked(t *testing.T) {
	tasks := []control.Task{
		{ID: "t_active", Title: "running one", Status: "in_progress"},
		{ID: "t_parked", Title: "parked one", Status: "in_progress"},
		{ID: "t_done", Title: "finished one", Status: "completed"},
	}
	out := formatTasks(tasks, "t_active")
	lines := strings.Split(out, "\n")
	if strings.Contains(lines[0], "paused") {
		t.Fatalf("the active task must not be marked paused: %q", lines[0])
	}
	if !strings.Contains(lines[1], "paused") {
		t.Fatalf("a parked non-active task must be marked paused: %q", lines[1])
	}
	if strings.Contains(lines[2], "paused") {
		t.Fatalf("a terminal task must not be marked paused: %q", lines[2])
	}
}
