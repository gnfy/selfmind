package verification

import "testing"

func TestCiteableDiscoveryUsesAcceptanceRules(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(*Check)
		mutations []Mutation
		want      int
	}{
		{"confirmed negative", nil, nil, 1},
		{"confirmed positive", func(c *Check) { c.Status = "succeeded"; c.ExitCode = 0 }, nil, 1},
		{"historical", func(c *Check) { c.Source = &CheckSource{RunID: "parent"} }, nil, 0},
		{"unknown effect", func(c *Check) { c.EffectState = "unknown" }, nil, 0},
		{"missing exit", func(c *Check) { c.ExitCode = -1 }, nil, 0},
		{"not dispatched", func(c *Check) { v := false; c.Invoked = &v }, nil, 0},
		{"stale", nil, []Mutation{{Path: "/workspace/config", FinishedAt: 20}}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := observedAssessmentCheck("issued-id", "step", "config", 1)
			if test.change != nil {
				test.change(&c)
			}
			facts, err := CiteableChecks([]Check{c}, test.mutations)
			if err != nil || len(facts) != test.want {
				t.Fatalf("facts=%+v err=%v", facts, err)
			}
			if len(facts) > 0 {
				_, err := ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: []string{facts[0].ID}, Reason: "Observed a different value."}}, []Check{c}, test.mutations, map[string]AssessmentRequirement{"step": {Criterion: "report result"}})
				if err != nil {
					t.Fatal("discovery offered an unusable reference:", err)
				}
			}
		})
	}
	a := observedAssessmentCheck("same", "step", "target", 1)
	b := observedAssessmentCheck("same", "other-step", "other-target", 0)
	if _, err := CiteableChecks([]Check{a, b}, nil); err == nil {
		t.Fatal("ambiguous id advertised as citeable")
	}
}
