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
		{"future contract", func(c *Check) { c.Binding.Version = 2 }, "failed"},
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
