package verification

import "testing"

func TestReplacementRequiresSameProofObligation(t *testing.T) {
	old := Check{ToolCallID: "old", Command: "broken parser", CWD: "/workspace", Status: "failed", StartedAt: 10, FinishedAt: 20, Binding: &Binding{Version: 1, Criterion: "all records exported", Target: "report.csv"}}
	for _, tc := range []struct {
		name  string
		alter func(*Check)
		want  string
	}{
		{"corrected method", func(*Check) {}, "passed"},
		{"different target", func(c *Check) { c.Binding.Target = "other.csv" }, "failed"},
		{"weaker condition", func(c *Check) { c.Binding.Criterion = "file exists" }, "failed"},
		{"different scope", func(c *Check) { c.CWD = "/other" }, "failed"},
		{"missing reason", func(c *Check) { c.Binding.Reason = "" }, "failed"},
		{"foreign reference", func(c *Check) { c.Binding.Replaces = "foreign" }, "failed"},
		{"new plan association", func(c *Check) { c.Binding.Version, c.Binding.StepID = 3, "later-step" }, "passed"},
		{"before old attempt finished", func(c *Check) { c.StartedAt = 15 }, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := Check{ToolCallID: "new", Command: "correct parser", CWD: "/workspace", Status: "succeeded", StartedAt: 30, FinishedAt: 40, Binding: &Binding{Version: 1, Criterion: "all records exported", Target: "report.csv", Replaces: "old", Reason: "Correct header handling; all data rows still required."}}
			tc.alter(&next)
			if got, _ := State(0, []Check{old, next}); got != tc.want {
				t.Fatalf("state=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestReplacementMayAssociateSameObligationWithLaterPlanStep(t *testing.T) {
	deps := []string{"/workspace/setting.txt"}
	old := Check{ToolCallID: "old", CWD: "/workspace", FinishedAt: 20, Binding: &Binding{Version: 3, StepID: "step-one", Criterion: "setting is positive", Target: "setting.txt", LocalDependencies: &deps}}
	next := Check{ToolCallID: "next", CWD: "/workspace", StartedAt: 30, Binding: &Binding{Version: 3, StepID: "step-two", Criterion: "setting is positive", Target: "setting.txt", LocalDependencies: &deps, Replaces: "old", Reason: "input changed; recheck the same condition"}}
	if mismatch := ReplacementMismatch(old, next); mismatch != "" {
		t.Fatalf("cross-step recheck rejected: %s", mismatch)
	}
	next.Binding.Target = "another.txt"
	if mismatch := ReplacementMismatch(old, next); mismatch == "" {
		t.Fatal("changed target accepted")
	}
}

func TestReplacementDoesNotRewriteHistoryOrDistinctFailure(t *testing.T) {
	old := Check{ToolCallID: "old", Command: "broken", Status: "failed", FinishedAt: 1, Binding: &Binding{Version: 1, Criterion: "complete", Target: "artifact"}}
	next := Check{ToolCallID: "next", Command: "fixed", Status: "succeeded", StartedAt: 2, FinishedAt: 3, Binding: &Binding{Version: 1, Criterion: "complete", Target: "artifact", Replaces: "old", Reason: "method corrected"}}
	checks := []Check{old, next, {Command: "other test", Status: "failed", StartedAt: 2, FinishedAt: 3}}
	if got, _ := State(0, checks); got != "failed" {
		t.Fatal(got)
	}
	if len(checks) != 3 || checks[0].Status != "failed" {
		t.Fatal("history mutated")
	}
	old.Binding = nil
	if got, _ := State(0, []Check{old, next}); got != "failed" {
		t.Fatal("historical evidence acquired replacement authority")
	}
}

func TestLatestPlanStepAttemptOwnsEffectiveObligationState(t *testing.T) {
	failed := Check{ToolCallID: "failed", Command: "unsupported reader", CWD: "/workspace", Status: "failed", StartedAt: 10, FinishedAt: 20, Binding: &Binding{Version: 3, StepID: "step-runtime", Criterion: "remote state is merged", Target: "step-runtime"}}
	passed := Check{ToolCallID: "passed", Command: "supported API", CWD: "/workspace", Status: "succeeded", StartedAt: 30, FinishedAt: 40, Binding: &Binding{Version: 3, StepID: "step-runtime", Criterion: "remote state is merged", Target: "step-runtime"}}
	if got, _ := State(0, []Check{failed, passed}); got != "passed" {
		t.Fatalf("corrected attempt left the obligation poisoned: %s", got)
	}
	if len(Latest([]Check{failed, passed})) != 1 {
		t.Fatal("one plan obligation projected more than one effective attempt")
	}

	distinct := passed
	distinct.Binding = &Binding{Version: 3, StepID: "step-other", Criterion: "another condition", Target: "step-other"}
	if got, _ := State(0, []Check{failed, distinct}); got != "failed" {
		t.Fatalf("unrelated plan evidence hid a failure: %s", got)
	}
}
