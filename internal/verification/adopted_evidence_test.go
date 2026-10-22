package verification

import "testing"

func TestReplacementReferencesStayWithinObservationRun(t *testing.T) {
	old := Check{ToolCallID: "check", Kind: "custom", Command: "check source", CWD: "/workspace", Status: "succeeded", StartedAt: 10, FinishedAt: 11,
		Source:  &CheckSource{RunID: "parent", StepID: "source-step", PlanVersion: 1},
		Binding: &Binding{Version: 3, StepID: "step", Criterion: "ready", Target: "input"}}
	current := Check{ToolCallID: "next", Kind: "custom", Command: "check new target", CWD: "/workspace", Status: "failed", StartedAt: 20, FinishedAt: 21,
		Binding: &Binding{Version: 1, Criterion: "ready", Target: "input", Replaces: "check", Reason: "new method"}}
	if checks := Latest([]Check{old, current}); len(checks) != 2 {
		t.Fatalf("child reference replaced a source-Run observation: %+v", checks)
	}
	// The same reference still replaces within its own original Run.
	current.Source = &CheckSource{RunID: "parent"}
	if checks := Latest([]Check{old, current}); len(checks) != 1 || checks[0].Status != "failed" {
		t.Fatalf("same-Run replacement stopped working: %+v", checks)
	}
}

func TestDispatchFactsPreserveRefusalsAndUnknownExits(t *testing.T) {
	started, refused, approved, rejected := true, false, true, false
	zero, failure := 0, 1
	for _, tc := range []struct {
		name  string
		check Check
		exit  *int
		want  string
	}{
		{"historical", Check{Status: "succeeded"}, nil, "succeeded"},
		{"started", Check{Status: "succeeded", ProcessStarted: &started, Invoked: &approved}, &zero, "succeeded"},
		{"failed", Check{Status: "succeeded", ProcessStarted: &started}, &failure, "failed"},
		{"unknown exit", Check{Status: "succeeded", ProcessStarted: &started}, nil, "blocked"},
		{"refused preparation", Check{Status: "succeeded", ProcessStarted: &refused}, &zero, "blocked"},
		{"not invoked", Check{Status: "succeeded", Invoked: &rejected}, nil, "blocked"},
		{"dispatch not proven", Check{Status: "succeeded", Invoked: &approved}, nil, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithDispatchFacts(tc.check, tc.exit); got.Status != tc.want {
				t.Fatalf("dispatch verdict = %+v, want %s", got, tc.want)
			}
		})
	}
}
