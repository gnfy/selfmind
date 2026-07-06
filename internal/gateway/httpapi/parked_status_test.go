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

// TestTaskCardStatusMapping: the /tasks card bracket is the simplified state —
// running (live run) beats everything, terminal statuses render verbatim,
// pending approvals/questions or blocked read as waiting, and every other open
// state (in_progress, interrupted, new) reads as paused.
func TestTaskCardStatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		task      control.Task
		isActive  bool
		approvals int
		questions int
		want      string
	}{
		{"live run wins", control.Task{Status: "in_progress"}, true, 0, 0, "running"},
		{"live run beats pending approval", control.Task{Status: "in_progress"}, true, 2, 0, "running"},
		{"pending approval waits", control.Task{Status: "in_progress"}, false, 1, 0, "waiting"},
		{"pending question waits", control.Task{Status: "in_progress"}, false, 0, 1, "waiting"},
		{"blocked waits", control.Task{Status: "blocked"}, false, 0, 0, "waiting"},
		{"parked in_progress pauses", control.Task{Status: "in_progress"}, false, 0, 0, "paused"},
		{"interrupted pauses", control.Task{Status: "interrupted"}, false, 0, 0, "paused"},
		{"new pauses", control.Task{Status: "new"}, false, 0, 0, "paused"},
		{"done verbatim", control.Task{Status: "done"}, false, 0, 0, "done"},
		{"completed verbatim", control.Task{Status: "completed"}, false, 0, 0, "completed"},
		{"cancelled verbatim", control.Task{Status: "cancelled"}, false, 0, 0, "cancelled"},
		{"archived verbatim", control.Task{Status: "archived"}, false, 0, 0, "archived"},
		{"terminal ignores stale pending", control.Task{Status: "done"}, false, 1, 0, "done"},
	}
	for _, tc := range cases {
		if got := taskCardStatus(tc.task, tc.isActive, tc.approvals, tc.questions); got != tc.want {
			t.Errorf("%s: taskCardStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}
