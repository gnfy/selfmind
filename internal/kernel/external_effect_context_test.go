package kernel

import (
	"fmt"
	"strings"
	"testing"
)

func TestSelectedBlockersStayCompleteAndDoNotBecomePlanWork(t *testing.T) {
	var items []ExternalEffectContext
	for i := 0; i < 5; i++ {
		items = append(items, ExternalEffectContext{ClaimID: fmt.Sprintf("claim-%d", i), RunID: "source", State: "uncertain", Target: "registry"})
	}
	r := TaskRuntimeContext{ExternalEffects: items}
	first := r.Prompt(10000)
	for _, want := range []string{"claim-3", "claim-4", "separate from Plan milestones", "effect_claim_id", "Approval of another action never settles"} {
		if !strings.Contains(first, want) {
			t.Fatalf("missing %q: %s", want, first)
		}
	}
	if first != r.Prompt(10000) {
		t.Fatal("blocker projection changed between model calls")
	}
}

func TestSelectedPriorEvidenceSurvivesSliceBudget(t *testing.T) {
	r := TaskRuntimeContext{}
	for i := 0; i < 16; i++ {
		r.InheritedEvidence = append(r.InheritedEvidence, InheritedEvidenceItem{StepID: fmt.Sprintf("step-%d", i), SourceRunID: "source"})
	}
	r.InheritedEvidence[15].LatestCheck = "succeeded"
	if got := r.Prompt(10000); !strings.Contains(got, "step-0") || !strings.Contains(got, "step-15") {
		t.Fatalf("silently omitted evidence: %s", got)
	}
	if got := r.Prompt(10000); !strings.Contains(got, "step-15") || r.InheritedEvidence[0].StepID != "step-0" {
		t.Fatal("empty references displaced observed proof, or rendering rewrote the selected context")
	}
}
