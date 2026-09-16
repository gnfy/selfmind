package tools

import (
	"strings"
	"testing"
)

func TestTriageUsesAttributedHumanEvidenceOnSystemResume(t *testing.T) {
	intent := RunIntentSnapshot{Source: "system:watch", RawUserText: "The prerequisite completed.", AuthorizationEvidence: []AuthorizationEvidence{{RunID: "run_parent", UserText: "Go ahead", AcceptedOffer: "Inspect and repair receipt.txt in this workspace."}}, AddedRequirements: []string{"Do not modify config.yaml."}}
	prompt := buildTriagePromptWithIntent("terminal", "cat receipt.txt", "host execution", intent)
	for _, want := range []string{"run_parent", "Go ahead", "Inspect and repair receipt.txt", "Do not modify config.yaml", "<system_request>", "does not erase earlier human authorization", "distinguish missing authorization from an execution permission boundary"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in prompt", want)
		}
	}
	if strings.Contains(prompt, "<person_asked>\nThe prerequisite") {
		t.Fatal("system notification impersonated user")
	}
}

func TestTriageDecisionRechecksChangedHumanEvidence(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	judge := &fakeJudge{reply: "APPROVE"}
	reads := 0
	scope := ExecutionScope{TenantID: "tenant-a", PersonID: "person-a", TaskID: "task-1", RunID: "run-1", ApprovalMode: ApprovalSmart, Judge: judge,
		IntentSnapshot: func() RunIntentSnapshot {
			reads++
			if reads == 1 {
				return RunIntentSnapshot{RawUserText: "Make the script executable", Source: "direct"}
			}
			return RunIntentSnapshot{RawUserText: "Make the script executable", Source: "direct", AddedRequirements: []string{"Limit the remaining work to inspection"}, AddedRequirementIDs: []string{"correction-1"}}
		},
	}
	for _, err := range runSmartInOneRun(t, scope, "person-a", "chmod +x run.sh", "chmod +x run.sh", "chmod +x run.sh") {
		if err != nil {
			t.Fatal(err)
		}
	}
	if judge.calls != 2 {
		t.Fatalf("changed evidence bypassed judgment or unchanged evidence was not reused: calls=%d", judge.calls)
	}
}
