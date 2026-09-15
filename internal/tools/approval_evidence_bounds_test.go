package tools

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestApprovalEvidenceRetainsWholeProposalAndBoundsOverflow(t *testing.T) {
	offer := strings.Repeat("核验完成。", 500) + "Three optional items: leave them alone."
	intent := RunIntentSnapshot{RawUserText: "Proceed except those three items.", PriorAssistantOffer: offer}
	prompt := buildTriagePromptWithIntent("terminal", "cat receipt.txt", "host", intent)
	if !strings.Contains(prompt, offer) {
		t.Fatal("judge lost the referenced optional items")
	}
	intent.AddedRequirements = []string{strings.Repeat("约束", 20000)}
	bounded := BoundApprovalEvidence(intent)
	if !bounded.AuthorizationEvidenceIncomplete || !utf8.ValidString(bounded.AddedRequirements[0]) {
		t.Fatal("truncation lost its incomplete marker or broke UTF-8")
	}
	if bounded.PriorAssistantOffer != offer || intent.AddedRequirements[0] == bounded.AddedRequirements[0] {
		t.Fatal("bounding mutated original evidence or removed complete proposal")
	}
	judge := &fakeJudge{reply: "APPROVE"}
	verdict, assessment, err := triageApprovalWithIntent(context.Background(), judge, "terminal", "cat receipt.txt", "host", intent)
	if err != nil || verdict != TriageEscalate || assessment.Outcome != "escalate" {
		t.Fatalf("partial evidence approved: %v %+v %v", verdict, assessment, err)
	}
}
