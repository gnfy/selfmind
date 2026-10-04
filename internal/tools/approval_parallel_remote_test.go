package tools

import (
	"context"
	"maps"
	"strings"
	"testing"
)

type countingApprovalJudge struct{ called int }

func (j *countingApprovalJudge) Judge(context.Context, string) (string, error) {
	j.called++
	return "APPROVE", nil
}

func (j *countingApprovalJudge) calls() int { return j.called }

func TestParallelRemoteExecRequiresOneShotHumanApprovalInEveryMode(t *testing.T) {
	withExecSandboxPolicy(t, true, true, true)
	for _, mode := range []ApprovalMode{ApprovalFullAuto, ApprovalSmart} {
		t.Run(string(mode), func(t *testing.T) {
			judge := &countingApprovalJudge{}
			asks, ran := 0, 0
			person := "parallel-remote-" + string(mode)
			cleanup := SetExecutionScope(person, ExecutionScope{
				TenantID: "tenant", PersonID: person, RunID: "run-remote", ParallelWork: true,
				ApprovalMode: mode, Judge: judge,
				Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks++
					if req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly || req.GrantClass != "" || len(req.RuleCandidates) != 0 {
						t.Fatalf("remote ask offered reusable authority: %+v", req)
					}
					if !strings.Contains(req.Reason, "shared network") {
						t.Fatalf("remote ask did not explain the boundary: %q", req.Reason)
					}
					return ToolApprovalDecision{Approved: true}, nil
				},
			})
			defer cleanup()
			exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
				ran++
				return "ran", nil
			})
			for range 2 {
				_, err := exec(map[string]interface{}{
					"_tenant_id": person, "_tool_name": "terminal", "command": "custom-deploy --target prod",
					"_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": true,
				})
				if err != nil {
					t.Fatalf("approved remote operation: %v", err)
				}
			}
			if asks != 2 || ran != 2 || judge.calls() != 0 {
				t.Fatalf("one-shot human boundary bypassed: asks=%d ran=%d judge=%d", asks, ran, judge.calls())
			}
		})
	}
}

func TestParallelRemoteApprovalPreservesObservationAndFailsClosedWithoutHumanChannel(t *testing.T) {
	withExecSandboxPolicy(t, true, true, true)
	person := "parallel-remote-observation"
	cleanup := SetExecutionScope(person, ExecutionScope{
		TenantID: "tenant", PersonID: person, RunID: "run-remote", ParallelWork: true,
		ApprovalMode: ApprovalFullAuto,
	})
	defer cleanup()
	ran := 0
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		ran++
		return "ran", nil
	})
	base := map[string]interface{}{
		"_tenant_id": person, "_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
		"_network_shared": true, credentialReadArgKey: true,
	}
	read := maps.Clone(base)
	read["command"] = "gcloud builds describe build-123"
	if _, err := exec(read); err != nil || ran != 1 {
		t.Fatalf("proven observation was blocked: ran=%d err=%v", ran, err)
	}
	write := maps.Clone(base)
	write["command"] = "custom-deploy --target prod"
	if _, err := exec(write); err == nil || !strings.Contains(err.Error(), "human approval channel") || ran != 1 {
		t.Fatalf("remote effect ran without a human channel: ran=%d err=%v", ran, err)
	}
}
