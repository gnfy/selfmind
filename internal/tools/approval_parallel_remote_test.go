package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// D7: a parallel Run approves shared-network and credential-bearing commands
// exactly as a single Run does in the same mode. Full-auto runs them; smart
// lets the judge decide and asks a person only when the judge escalates,
// fails, or times out. Cross-Run protection is the external-effect claim, not
// a parallel-only ask.
func TestParallelRemoteExecFollowsTheRunApprovalMode(t *testing.T) {
	withExecSandboxPolicy(t, true, true, true)
	commands := []map[string]interface{}{
		{"command": "custom-deploy --target prod", "_network_shared": true},
		{"command": "aws s3 cp build.tar s3://releases/build.tar", "_network_shared": true, credentialReadArgKey: true},
	}
	for _, tc := range []struct {
		name      string
		mode      ApprovalMode
		judge     *fakeJudge
		wantAsks  int
		wantJudge bool
	}{
		{name: "full-auto", mode: ApprovalFullAuto, judge: &fakeJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"asked for"}`}},
		{name: "smart judge approves", mode: ApprovalSmart, wantJudge: true,
			judge: &fakeJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"The person asked for the deploy."}`}},
		{name: "smart judge fails", mode: ApprovalSmart, wantJudge: true, wantAsks: len(commands),
			judge: &fakeJudge{err: errors.New("judge provider unavailable")}},
	} {
		for _, parallel := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s parallel=%v", tc.name, parallel), func(t *testing.T) {
				judge := *tc.judge
				asks, ran := 0, 0
				person := fmt.Sprintf("parallel-remote-%s-%v", strings.ReplaceAll(tc.name, " ", "-"), parallel)
				cleanup := SetExecutionScope(person, ExecutionScope{
					TenantID: "tenant", PersonID: person, RunID: "run-" + person, ParallelWork: parallel,
					ApprovalMode: tc.mode, Judge: &judge,
					Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
						asks++
						if strings.Contains(req.Reason, "parallel") {
							t.Fatalf("ask carried a parallel-only reason: %q", req.Reason)
						}
						return ToolApprovalDecision{Approved: true}, nil
					},
				})
				defer cleanup()
				exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
					ran++
					return "ran", nil
				})
				for _, command := range commands {
					args := map[string]interface{}{"_tenant_id": person, "_tool_name": "terminal",
						"_effective_sandbox_mode": string(SandboxIsolated)}
					for key, value := range command {
						args[key] = value
					}
					if _, err := exec(args); err != nil {
						t.Fatalf("%v: %v", command["command"], err)
					}
				}
				if asks != tc.wantAsks || ran != len(commands) || (judge.calls > 0) != tc.wantJudge {
					t.Fatalf("asks=%d ran=%d judge=%d; want asks=%d judge=%v", asks, ran, judge.calls, tc.wantAsks, tc.wantJudge)
				}
			})
		}
	}
}

// Without a human channel a parallel full-auto Run still runs its remote
// command and proven observation: nothing parallel-specific asks for one.
func TestParallelRemoteExecNeedsNoHumanChannelInFullAuto(t *testing.T) {
	withExecSandboxPolicy(t, true, true, true)
	person := "parallel-remote-no-channel"
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
	for _, command := range []string{"gcloud builds describe build-123", "custom-deploy --target prod"} {
		if _, err := exec(map[string]interface{}{
			"_tenant_id": person, "_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, credentialReadArgKey: true, "command": command,
		}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	if ran != 2 {
		t.Fatalf("ran=%d", ran)
	}
}
