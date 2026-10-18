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
	corrected := false
	scope := ExecutionScope{TenantID: "tenant-a", PersonID: "person-a", TaskID: "task-1", RunID: "run-1", ApprovalMode: ApprovalSmart, Judge: judge,
		IntentSnapshot: func() RunIntentSnapshot {
			if !corrected {
				return RunIntentSnapshot{RawUserText: "Make the script executable", Source: "direct"}
			}
			return RunIntentSnapshot{RawUserText: "Make the script executable", Source: "direct", AddedRequirements: []string{"Limit the remaining work to inspection"}, AddedRequirementIDs: []string{"correction-1"}}
		},
	}
	defer SetExecutionScope("person-a", scope)()
	for i := 0; i < 3; i++ {
		// A new person input arrives between calls, not because a consumer reads
		// the snapshot again to fence a pending review.
		corrected = i > 0
		ran := false
		exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran = true; return "ok", nil })
		_, err := exec(map[string]interface{}{"_tenant_id": "person-a", "_tool_name": "terminal", "command": "chmod +x run.sh"})
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			t.Fatal("approved action did not run")
		}
	}
	if judge.calls != 2 {
		t.Fatalf("changed evidence bypassed judgment or unchanged evidence was not reused: calls=%d", judge.calls)
	}
}
