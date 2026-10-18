package verification

import "testing"

func TestStepAssessmentPreservesSourceWithoutCoveringCurrentIDCollision(t *testing.T) {
	adopted := observedAssessmentCheck("same-id", "selected-step", "target-a", 0)
	adopted.Source = &CheckSource{RunID: "parent", StepID: "source-step", PlanVersion: 2, Reason: "Explicitly adopted."}
	current := observedAssessmentCheck("same-id", "other-step", "target-b", 1)
	req := map[string]AssessmentRequirement{"selected-step": {Criterion: "A is ready", PositiveRequired: true}, "other-step": {Criterion: "report B"}}
	p, err := ProjectAssessments("current", []Assessment{{StepID: "selected-step", Result: "met", Reason: "The adopted proof suffices."}}, []Check{adopted, current}, nil, req)
	if err != nil || len(p.Conditions) != 1 || p.Conditions[0].Version != 2 || p.Conditions[0].Source == nil || p.Conditions[0].Source.RunID != "parent" || p.ReviewComplete {
		t.Fatalf("source lost or current failure covered by colliding id: %+v %v", p, err)
	}
	if _, err := ProjectAssessments("current", p.Conditions, []Check{adopted, current}, nil, req); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Assessment){
		func(a *Assessment) { a.Source.RunID = "foreign" }, func(a *Assessment) { a.Source.StepID = "foreign" }, func(a *Assessment) { a.Source.PlanVersion++ }, func(a *Assessment) { a.Target = "other" }, func(a *Assessment) { a.Version = 1 },
	} {
		a := p.Conditions[0]
		source := *a.Source
		a.Source = &source
		change(&a)
		if _, err := ProjectAssessments("current", []Assessment{a}, []Check{adopted, current}, nil, req); err == nil {
			t.Fatal("changed source or identity accepted")
		}
	}
}

func TestStepAssessmentRefusesAmbiguityAndMixedSelectors(t *testing.T) {
	a := observedAssessmentCheck("a", "step", "target-a", 0)
	b := observedAssessmentCheck("b", "step", "target-b", 0)
	for _, input := range []Assessment{
		{StepID: "step", Result: "met", Reason: "Both."},
		{StepID: "step", EvidenceRefs: []string{"a"}, Result: "met", Reason: "Conflicting selectors."},
		{StepID: "missing", Result: "met", Reason: "Unknown step."},
		{StepID: "step", Result: "met", Reason: "Forged adoption.", Source: &CheckSource{RunID: "made-up"}},
	} {
		if _, err := ProjectAssessments("run", []Assessment{input}, []Check{a, b}, nil, map[string]AssessmentRequirement{"step": {}}); err == nil {
			t.Fatalf("ambiguous or forged selection accepted: %+v", input)
		}
	}
	// The precise current-Run route still works for one of several targets.
	if _, err := ProjectAssessments("run", []Assessment{{EvidenceRefs: []string{"a"}, Result: "met", Reason: "Only A."}}, []Check{a, b}, nil, map[string]AssessmentRequirement{"step": {}}); err != nil {
		t.Fatal(err)
	}
}

func TestStepAssessmentCannotUseUnconfirmedOrStaleChecks(t *testing.T) {
	for _, name := range []string{"legacy-missing-process-facts", "not-invoked", "not-started", "unknown-exit", "unknown-effect", "stale"} {
		t.Run(name, func(t *testing.T) {
			c := observedAssessmentCheck("check", "step", "target", 0)
			var mutations []Mutation
			no := false
			switch name {
			case "legacy-missing-process-facts":
				c.Invoked, c.ProcessStarted = nil, nil
			case "not-invoked":
				c.Invoked = &no
			case "not-started":
				c.ProcessStarted = &no
			case "unknown-exit":
				c.ExitCode = -1
			case "unknown-effect":
				c.EffectState = "unknown"
			case "stale":
				mutations = []Mutation{{Path: "/workspace/target", FinishedAt: 20}}
			}
			if _, err := ProjectAssessments("run", []Assessment{{StepID: "step", Result: "met", Reason: "Observed."}}, []Check{c}, mutations, map[string]AssessmentRequirement{"step": {}}); err == nil {
				t.Fatal("step selection bypassed execution or freshness facts")
			}
		})
	}
}

func TestStepAssessmentCurrentNegativeReviewCanFinishInspection(t *testing.T) {
	check := observedAssessmentCheck("current", "step", "config", 1)
	p, err := ProjectAssessments("run", []Assessment{{StepID: "step", Result: "not_met", Reason: "The value was inspected and differs."}}, []Check{check}, nil, map[string]AssessmentRequirement{"step": {Criterion: "inspect and report"}})
	if err != nil || !p.ReviewComplete || len(p.Conditions) != 1 || p.Conditions[0].Version != 1 || p.Conditions[0].Source != nil {
		t.Fatalf("current negative inspection was forced into remediation or relabelled: %+v %v", p, err)
	}
	if _, err := ProjectAssessments("run", p.Conditions, []Check{check}, nil, map[string]AssessmentRequirement{"step": {Criterion: "inspect and report"}}); err != nil {
		t.Fatal(err)
	}
}

func TestStepAssessmentAcceptsCompatibleStepAndEvidenceRefsAndDeduplicatesIdenticalFindings(t *testing.T) {
	adopted := observedAssessmentCheck("parent-check", "adopted-step", "target-a", 0)
	adopted.Source = &CheckSource{RunID: "parent", StepID: "adopted-step", PlanVersion: 1, Reason: "Adopted."}
	current := observedAssessmentCheck("toolcall-15-0", "current-step", "target-b", 0)
	req := map[string]AssessmentRequirement{
		"adopted-step": {Criterion: "A is ready", PositiveRequired: true},
		"current-step": {Criterion: "B is ready", PositiveRequired: true},
	}
	p, err := ProjectAssessments("run", []Assessment{
		{StepID: "adopted-step", EvidenceRefs: []string{"toolcall-15-0"}, Result: "met", Reason: "Adopted step holds."},
		{StepID: "current-step", EvidenceRefs: []string{"toolcall-15-0"}, Result: "met", Reason: "Current step holds."},
	}, []Check{adopted, current}, nil, req)
	if err != nil || !p.ReviewComplete || len(p.Conditions) != 2 {
		t.Fatalf("compatible step_id + evidence_refs was rejected: %+v %v", p, err)
	}
	// Duplicate identical findings with the same evidence_refs and result are deduplicated.
	p2, err := ProjectAssessments("run", []Assessment{
		{EvidenceRefs: []string{"toolcall-15-0"}, Result: "met", Reason: "First copy."},
		{EvidenceRefs: []string{"toolcall-15-0"}, Result: "met", Reason: "Second copy."},
	}, []Check{current}, nil, map[string]AssessmentRequirement{"current-step": {Criterion: "B is ready", PositiveRequired: true}})
	if err != nil || !p2.ReviewComplete || len(p2.Conditions) != 1 {
		t.Fatalf("identical duplicate assessments were not deduplicated: %+v %v", p2, err)
	}
	// A failed check followed by a succeeded check on the same step does not make the succeeded check ID ambiguous,
	// and a redundant summary assessment targeting a non-check step while citing already-covered evidence_refs is skipped.
	failedFirst := observedAssessmentCheck("toolcall-25-0", "current-step", "target-b", 1)
	failedFirst.StartedAt, failedFirst.FinishedAt = 5, 6
	passedRetry := observedAssessmentCheck("toolcall-26-0", "current-step", "target-b", 0)
	passedRetry.StartedAt, passedRetry.FinishedAt = 10, 11
	reqWithSummary := map[string]AssessmentRequirement{
		"current-step": {Criterion: "B is ready", PositiveRequired: true},
		"summary-step": {Criterion: "All steps summarized"},
	}
	p3, err := ProjectAssessments("run", []Assessment{
		{StepID: "current-step", EvidenceRefs: []string{"toolcall-26-0"}, Result: "met", Reason: "Retry succeeded."},
		{StepID: "summary-step", EvidenceRefs: []string{"toolcall-26-0"}, Result: "met", Reason: "Summary of retry."},
	}, []Check{failedFirst, passedRetry}, nil, reqWithSummary)
	if err != nil || !p3.ReviewComplete || len(p3.Conditions) != 1 || p3.Conditions[0].EvidenceRefs[0] != "toolcall-26-0" {
		t.Fatalf("retry on same step or redundant summary failed: %+v %v", p3, err)
	}
}

func TestStepAssessmentCoversMultipleChecksOnSameStepWhenDefaultOrAllEvidenceRefsCited(t *testing.T) {
	defaultCheck := observedAssessmentCheck("toolcall-16-0", "step-3", "step-3", 0)
	defaultCheck.StartedAt, defaultCheck.FinishedAt = 10, 11
	specificCheck := observedAssessmentCheck("toolcall-26-0", "step-3", "artifact-bundle", 0)
	specificCheck.StartedAt, specificCheck.FinishedAt = 20, 21
	req := map[string]AssessmentRequirement{
		"step-3": {Criterion: "Bundle is complete and verified", PositiveRequired: true},
	}

	// Single step assessment citing the latest specific check covers both the default target check and the specific target check.
	p, err := ProjectAssessments("run", []Assessment{
		{StepID: "step-3", EvidenceRefs: []string{"toolcall-26-0"}, Result: "met", Reason: "Bundle verified."},
	}, []Check{defaultCheck, specificCheck}, nil, req)
	if err != nil || !p.ReviewComplete || len(p.Conditions) != 1 {
		t.Fatalf("step assessment did not cover default + specific check on same step: %+v %v", p, err)
	}

	// Two distinct non-default targets on the same step require citing both evidence_refs if assessed in one entry.
	nonDefaultA := observedAssessmentCheck("toolcall-30-0", "step-3", "target-a", 0)
	nonDefaultB := observedAssessmentCheck("toolcall-31-0", "step-3", "target-b", 0)
	if _, err := ProjectAssessments("run", []Assessment{
		{StepID: "step-3", Result: "met", Reason: "Bare step_id across two non-default targets."},
	}, []Check{nonDefaultA, nonDefaultB}, nil, req); err == nil {
		t.Fatal("expected bare step_id across two distinct non-default targets to be rejected")
	}
	pBoth, err := ProjectAssessments("run", []Assessment{
		{StepID: "step-3", EvidenceRefs: []string{"toolcall-30-0", "toolcall-31-0"}, Result: "met", Reason: "Both targets verified."},
	}, []Check{nonDefaultA, nonDefaultB}, nil, req)
	if err != nil || !pBoth.ReviewComplete || len(pBoth.Conditions) != 1 {
		t.Fatalf("step assessment citing all active evidence_refs of the step was rejected: %+v %v", pBoth, err)
	}
}

func TestSharedObservationNeedsExplicitStepAndKeepsOriginalFacts(t *testing.T) {
	a := observedAssessmentCheck("source", "first", "target", 0)
	a.Observation = &Observation{Kind: "command", CWD: a.CWD}
	b := a
	binding := *a.Binding
	binding.StepID = "second"
	b.Binding = &binding
	checks := []Check{a, b}
	req := map[string]AssessmentRequirement{"first": {Criterion: a.Binding.Criterion}, "second": {Criterion: b.Binding.Criterion}}
	if _, err := ProjectAssessments("run", []Assessment{{StepID: "first", EvidenceRefs: []string{"source"}, Result: "met", Reason: "First condition"}, {StepID: "second", EvidenceRefs: []string{"source"}, Result: "met", Reason: "Second condition"}}, checks, nil, req); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectAssessments("run", []Assessment{{EvidenceRefs: []string{"source"}, Result: "met", Reason: "No step"}}, checks, nil, req); err == nil {
		t.Fatal("bare id selected an ambiguous condition")
	}
	b.ExitCode = 1
	if _, err := ProjectAssessments("run", []Assessment{{StepID: "first", Result: "met", Reason: "Cannot hide conflicting execution"}}, []Check{a, b}, nil, req); err == nil {
		t.Fatal("colliding source ids hid conflicting facts")
	}
}
