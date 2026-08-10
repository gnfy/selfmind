package eval

import (
	"testing"
	"time"
)

// max_duration_seconds is a REPLAY assertion. Recording the same case pays
// full model latency — measured at 288s live against 40s on replay for
// smoke_skill_architecture_007 — so holding a recording to the replay budget
// kills it part-way and leaves a truncated cassette.
func TestRecordingGetsItsOwnFloorButReplayKeepsTheCaseBudget(t *testing.T) {
	c := &Case{}
	c.Expect.MaxDurationSeconds = 120

	if got := resolveTurnBudget(c, RunOptions{}); got != 120*time.Second {
		t.Fatalf("replay budget = %s, want the case's own 120s", got)
	}

	t.Setenv("SELFMIND_EVAL_VCR", "record")
	if got := turnBudget(c, RunOptions{}); got != recordingTurnBudgetFloor {
		t.Fatalf("recording budget = %s, want the floor %s", got, recordingTurnBudgetFloor)
	}

	// An explicit override always wins, in either mode.
	if got := turnBudget(c, RunOptions{TurnTimeout: 42 * time.Minute}); got != 42*time.Minute {
		t.Fatalf("explicit budget = %s, want 42m", got)
	}
}

// A case that already asks for more than the floor keeps its own budget.
func TestRecordingFloorNeverShortensAGenerousBudget(t *testing.T) {
	t.Setenv("SELFMIND_EVAL_VCR", "record")
	c := &Case{}
	c.Expect.MaxDurationSeconds = int(recordingTurnBudgetFloor.Seconds()) + 600

	if got := turnBudget(c, RunOptions{}); got != time.Duration(c.Expect.MaxDurationSeconds)*time.Second {
		t.Fatalf("budget = %s, want the case's larger budget", got)
	}
}
