package verification

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func observedAssessmentCheck(id, step, target string, exit int) Check {
	started, invoked := true, true
	status := "succeeded"
	if exit != 0 {
		status = "failed"
	}
	return Check{ToolCallID: id, Status: status, ExitCode: exit, ProcessStarted: &started, Invoked: &invoked,
		Command: "observe", CWD: "/workspace", StartedAt: 10, FinishedAt: 11,
		Binding: &Binding{Version: 3, StepID: step, Criterion: "value is ready", Target: target}}
}

func TestConditionAssessmentSeparatesFactsFindingAndRequirement(t *testing.T) {
	for _, exit := range []int{0, 1} {
		for _, required := range []bool{false, true} {
			check := observedAssessmentCheck("check", "step", "config", exit)
			p, err := ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: []string{"check"}, Reason: "The observed value differs."}}, []Check{check}, nil,
				map[string]AssessmentRequirement{"step": {Criterion: "establish and report the value", PositiveRequired: required}})
			if err != nil || p.State != "not_met" || (len(p.Blockers) > 0) != required || p.ReviewComplete == required {
				t.Fatalf("exit=%d required=%v p=%+v err=%v", exit, required, p, err)
			}
			if p.Conditions[0].Version != 1 || p.Conditions[0].Target != "config" || p.Conditions[0].StepID != "step" {
				t.Fatalf("lost bound identity: %+v", p)
			}
			if check.ExitCode != exit || (check.Status == "failed") != (exit != 0) {
				t.Fatal("Main judgment rewrote execution")
			}
		}
	}
	check := observedAssessmentCheck("check", "step", "config", 0)
	p, err := ProjectAssessments("run", nil, []Check{check}, nil, nil)
	if err != nil || p.State != "unassessed" {
		t.Fatal("exit zero became a condition verdict")
	}
}

func TestConditionAssessmentsCannotStoreTwoConclusionsForOneCondition(t *testing.T) {
	a := observedAssessmentCheck("a", "step", "target", 0)
	b := observedAssessmentCheck("b", "step", "target", 0)
	inputs := []string{"other-input"}
	b.Binding.LocalDependencies = &inputs // Both methods remain effective.
	_, err := ProjectAssessments("run", []Assessment{
		{Result: "unknown", EvidenceRefs: []string{"a"}, Reason: "Undetermined."},
		{Result: "met", EvidenceRefs: []string{"b"}, Reason: "Positive."},
	}, []Check{a, b}, nil, map[string]AssessmentRequirement{"step": {Criterion: "target is ready"}})
	if err == nil {
		t.Fatal("conflicting findings could change meaning on save/reload")
	}
}

func TestConditionAssessmentRejectsUnusableEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		alter     func(*Check)
		refs      []string
		mutations []Mutation
	}{
		{"unknown ref", nil, []string{"foreign"}, nil},
		{"repeated ref", nil, []string{"check", "check"}, nil},
		{"not invoked", func(c *Check) { v := false; c.Invoked = &v }, nil, nil},
		{"not started", func(c *Check) { v := false; c.ProcessStarted = &v }, nil, nil},
		{"exit unknown", func(c *Check) { c.ExitCode = -1 }, nil, nil},
		{"unknown effects", func(c *Check) { c.EffectState = "unknown" }, nil, nil},
		{"preparation blocked", func(c *Check) { c.Status = "blocked" }, nil, nil},
		{"stale", nil, nil, []Mutation{{Path: "/workspace/config", FinishedAt: 20}}},
		{"prior run", func(c *Check) { c.Source = &CheckSource{RunID: "parent"} }, nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := observedAssessmentCheck("check", "step", "config", 1)
			if test.alter != nil {
				test.alter(&check)
			}
			refs := test.refs
			if refs == nil {
				refs = []string{"check"}
			}
			_, err := ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: refs, Reason: "Observed a difference."}}, []Check{check}, test.mutations, map[string]AssessmentRequirement{"step": {Criterion: "report value"}})
			if err == nil {
				t.Fatal("unusable evidence accepted")
			}
		})
	}
}

func TestConditionAssessmentDoesNotHideIndependentFailures(t *testing.T) {
	a := observedAssessmentCheck("a", "step-a", "target-a", 1)
	b := observedAssessmentCheck("b", "step-b", "target-b", 1)
	p, err := ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: []string{"a"}, Reason: "A is absent."}}, []Check{a, b}, nil, map[string]AssessmentRequirement{"step-a": {}, "step-b": {}})
	if err != nil || p.ReviewComplete {
		t.Fatalf("unrelated failure disappeared: %+v / %v", p, err)
	}
	_, err = ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: []string{"a", "b"}, Reason: "Both absent."}}, []Check{a, b}, nil, map[string]AssessmentRequirement{"step-a": {}, "step-b": {}})
	if err == nil {
		t.Fatal("different obligations merged")
	}
}

func TestConditionAssessmentKeepsIdentityAndInputFreshness(t *testing.T) {
	check := observedAssessmentCheck("check", "step", "config", 0)
	deps := []string{"/workspace/input"}
	check.Binding.LocalDependencies = &deps
	requirements := map[string]AssessmentRequirement{"step": {Criterion: "inspect value"}}
	input := []Assessment{{Result: "not_met", EvidenceRefs: []string{"check"}, Reason: "Value differs."}}
	p, err := ProjectAssessments("run", input, []Check{check}, []Mutation{{Path: "/workspace/report", FinishedAt: 20}}, requirements)
	if err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*Assessment){
		func(a *Assessment) { a.RunID = "foreign" }, func(a *Assessment) { a.Target = "other" }, func(a *Assessment) { a.Criterion = "weaker" }, func(a *Assessment) { a.CWD = "/other" }, func(a *Assessment) { a.PlanCriterion = "changed" },
	} {
		a := p.Conditions[0]
		alter(&a)
		if _, err := ProjectAssessments("run", []Assessment{a}, []Check{check}, nil, requirements); err == nil {
			t.Fatal("changed assessment identity accepted")
		}
	}
	next := check
	next.ToolCallID = "replacement"
	next.StartedAt = 30
	next.FinishedAt = 31
	if _, err := ProjectAssessments("run", input, []Check{check, next}, nil, requirements); err == nil {
		t.Fatal("superseded evidence accepted")
	}
}

// A refusal that only says "unavailable" leaves the model to guess the way out.
// The error names what can be cited: this Run's usable checks, in a stable order.
func TestUnavailableEvidenceListsOnlyWhatCanBeCited(t *testing.T) {
	usable := observedAssessmentCheck("b-usable", "step", "config", 1)
	other := observedAssessmentCheck("a-usable", "other-step", "report", 1)
	other.Binding.Criterion = "report exists"
	stale := observedAssessmentCheck("stale", "step", "late", 1)
	notRun := observedAssessmentCheck("not-run", "step", "never", 1)
	for _, c := range []Check{usable, other, notRun} {
		inputs := []string{"/workspace/input"}
		c.Binding.LocalDependencies = &inputs
	}
	changed := []string{"/workspace/late"}
	stale.Binding.LocalDependencies = &changed
	v := false
	notRun.ProcessStarted = &v
	requirements := map[string]AssessmentRequirement{"step": {Criterion: "report value"}, "other-step": {Criterion: "report exists"}}
	mutations := []Mutation{{Path: "/workspace/late", FinishedAt: 99}}
	_, err := ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: []string{"parent-check"}, Reason: "Seen earlier."}},
		[]Check{usable, other, stale, notRun}, mutations, requirements)
	var unavailable *EvidenceUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Ref != "parent-check" {
		t.Fatalf("not the unavailable-evidence error: %v", err)
	}
	var ids []string
	for _, c := range unavailable.Citeable {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "a-usable,b-usable" {
		t.Fatalf("citeable = %v, want only the usable checks in id order", ids)
	}
	for _, want := range []string{`"parent-check"`, "call ids restart in every Run", "a-usable (step other-step", "b-usable (step step"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message lacks %q: %v", want, err)
		}
	}

	_, err = ProjectAssessments("run", []Assessment{{Result: "not_met", EvidenceRefs: []string{"parent-check"}, Reason: "Seen earlier."}}, nil, nil, requirements)
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "no effective verify check yet") {
		t.Fatalf("a Run without checks should say so: %v", err)
	}
	unavailable.Note = "check-0 is the earlier-Run observation behind step s."
	if strings.Contains(unavailable.Error(), "no effective verify check yet") || !strings.Contains(unavailable.Error(), "earlier-Run observation behind step s.") {
		t.Fatalf("a note must replace the generic advice: %v", unavailable)
	}

	many := &EvidenceUnavailableError{Ref: "x"}
	for i := 0; i < evidenceUnavailableListed+3; i++ {
		many.Citeable = append(many.Citeable, CiteableEvidence{ID: string(rune('a' + i)), Target: "t"})
	}
	if !strings.Contains(many.Error(), "and 3 more") {
		t.Fatalf("a long list is not bounded: %v", many)
	}
}

func TestCrossDirectorySucceededCheckSupersedesEarlierCheckWithSameStepCriterionTargetAndInputs(t *testing.T) {
	deps := []string{"/workspace/input.json"}
	first := observedAssessmentCheck("check-repo-a", "step-1", "release_tag_comparison", 0)
	first.CWD = "/workspace/repo-a"
	first.StartedAt = 10
	first.FinishedAt = 11
	first.Binding.LocalDependencies = &deps

	second := observedAssessmentCheck("check-root", "step-1", "release_tag_comparison", 0)
	second.CWD = "/workspace"
	second.StartedAt = 20
	second.FinishedAt = 21
	second.Binding.LocalDependencies = &deps

	effective := Latest([]Check{first, second})
	if len(effective) != 1 || effective[0].ToolCallID != "check-root" {
		t.Fatalf("expected later check-root to supersede check-repo-a across CWD when step/criterion/target/inputs match, got %+v", effective)
	}
}

func TestDeduplicateRedundantSucceededChecksCoversSubsetAndSummaryChecks(t *testing.T) {
	fullDeps := []string{"/workspace/a.json", "/workspace/b.json"}
	subDeps := []string{"/workspace/a.json"}
	emptyDeps := []string{}

	full := observedAssessmentCheck("check-full", "step-1", "release_audit", 0)
	full.StartedAt = 10
	full.FinishedAt = 11
	full.Binding.LocalDependencies = &fullDeps

	sub := observedAssessmentCheck("check-sub", "step-1", "release_audit", 0)
	sub.StartedAt = 20
	sub.FinishedAt = 21
	sub.Binding.LocalDependencies = &subDeps

	summary := observedAssessmentCheck("check-summary", "step-1", "release_audit", 0)
	summary.StartedAt = 30
	summary.FinishedAt = 31
	summary.Binding.LocalDependencies = &emptyDeps

	requirements := map[string]AssessmentRequirement{"step-1": {Criterion: "value is ready"}}
	p, err := ProjectAssessments(
		"run",
		[]Assessment{
			{StepID: "step-1", Result: "met", EvidenceRefs: []string{"check-full", "check-sub", "check-summary"}, Reason: "All checks passed."},
			{StepID: "step-1", Result: "met", Reason: "Step summary."},
		},
		[]Check{full, sub, summary},
		nil,
		requirements,
	)
	if err != nil {
		t.Fatalf("ProjectAssessments failed: %v", err)
	}
	if !p.ReviewComplete || len(p.Blockers) != 0 {
		t.Fatalf("expected ReviewComplete with 0 blockers, got ReviewComplete=%v Blockers=%+v", p.ReviewComplete, p.Blockers)
	}
}

func TestUnusableObservationCausesAreTypedAndBounded(t *testing.T) {
	var checks []Check
	var assessments []Assessment
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("check-%d", i)
		step := fmt.Sprintf("step-%d", i)
		checks = append(checks, observedAssessmentCheck(id, step, "target", 0))
		assessments = append(assessments, Assessment{Result: "met", EvidenceRefs: []string{id}, Reason: "Observed"})
	}
	_, err := ProjectAssessments("run", assessments, checks, []Mutation{{Path: "/workspace/report", FinishedAt: 20}}, nil)
	var issues *EvidenceIssuesError
	if !errors.As(err, &issues) || len(issues.Issues) != 8 || issues.Omitted != 4 {
		t.Fatalf("lost bounded cause: %+v %v", issues, err)
	}
	for _, issue := range issues.Issues {
		if issue.Kind != "stale" || issue.StepID == "" || issue.CWD != "/workspace" {
			t.Fatalf("lost provenance: %+v", issue)
		}
	}
}
