package tools

import (
	"context"
	"strings"
	"testing"
)

func TestDenialMustCiteActualHumanEvidence(t *testing.T) {
	intent := RunIntentSnapshot{RawUserText: "Run the tests. Do not publish the package.", PriorAssistantOffer: "I can test and publish.", AddedRequirements: []string{"Do not change credentials."}}
	for _, tc := range []struct {
		source, quote string
		valid         bool
	}{
		{"person_asked", "Do not publish the package.", true},
		{"person_added:0", "Do not change credentials.", true},
		{"person_asked", "Only execute exactly the listed commands", false},
		{"person_added:1", "Do not change credentials.", false},
		{"system_request", "Do not publish the package.", false},
	} {
		err := validateRestrictionCitation(TriageAssessment{Outcome: "deny", Restriction: &RestrictionEvidence{Source: tc.source, Quote: tc.quote, AppliesTo: "the proposed effect"}}, intent)
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	// Existence of a quote does not prove semantic applicability. Live evals
	// distinguish an authorized test from the prohibited publication effect.
}

func TestIncompleteActionCannotBeApprovedFromPrefix(t *testing.T) {
	_, _, err := triageApprovalWithIntent(context.Background(), &fakeJudge{reply: "APPROVE"}, "terminal", strings.Repeat("x", triageMaxSubjectBytes+1), "review", RunIntentSnapshot{})
	if err == nil {
		t.Fatal("incomplete action was judged from prefix")
	}
}
