package tools

import (
	"context"
	"testing"
)

type resumeAuthorizationStub struct {
	claimed bool
	calls   int
}

func (s *resumeAuthorizationStub) ClaimApprovalResumeAuthorization(_ context.Context, _, _, _, _, _ string) (string, string, string, bool, error) {
	s.calls++
	if s.claimed {
		return "", "", "", false, nil
	}
	s.claimed = true
	return "apr_resume", "once", "", true, nil
}

func TestParkedApprovalAuthorizationRunsExactRegeneratedActionWithoutReasking(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	resume := &resumeAuthorizationStub{}
	asks := 0
	ran := 0
	cleanup := SetExecutionScope("person-resume", ExecutionScope{
		TenantID: "default", PersonID: "person-resume", TaskID: "task-resume", RunID: "run-resume",
		WorkspaceID: "ws-resume", WorkspaceRoot: t.TempDir(), ApprovalMode: ApprovalOnRequest,
		EnvironmentFingerprint: "env-fp", PrincipalFingerprint: "principal-fp", CredentialSourceHash: "cred-fp",
		ResumeAuthorizations: resume,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			return ToolApprovalDecision{Approved: false}, nil
		},
	})
	defer cleanup()
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		ran++
		return "ok", nil
	})
	ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run-resume"))
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-resume", "_context": ctx,
		"_tool_name": "terminal", "command": "git status",
	}); err != nil {
		t.Fatalf("regenerated action: %v", err)
	}
	if resume.calls != 1 || asks != 0 || ran != 1 {
		t.Fatalf("claims=%d asks=%d ran=%d, want 1/0/1", resume.calls, asks, ran)
	}
}

func TestParkedApprovalAuthorizationCannotBypassHardFloor(t *testing.T) {
	resume := &resumeAuthorizationStub{}
	ran := false
	cleanup := SetExecutionScope("person-resume-hard", ExecutionScope{
		TenantID: "default", PersonID: "person-resume-hard", TaskID: "task-resume-hard", RunID: "run-resume-hard",
		WorkspaceID: "ws-resume", WorkspaceRoot: t.TempDir(), ApprovalMode: ApprovalFullAuto,
		ResumeAuthorizations: resume,
	})
	defer cleanup()
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run-resume-hard"))
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-resume-hard", "_context": ctx,
		"_tool_name": "terminal", "command": "rm -rf /",
	}); err == nil {
		t.Fatal("hard-floor action unexpectedly succeeded")
	}
	if resume.calls != 0 || ran {
		t.Fatalf("hard floor must run before resume capability: claims=%d ran=%v", resume.calls, ran)
	}
}

func TestApprovalResumeFingerprintBindsActionAndStableExecutionIdentity(t *testing.T) {
	base := ExecutionScope{
		TenantID: "default", PersonID: "person", TaskID: "task", WorkspaceID: "workspace",
		EnvironmentFingerprint: "env", PrincipalFingerprint: "principal", CredentialSourceHash: "cred",
	}
	one := approvalResumeFingerprint("terminal", map[string]interface{}{"command": "git status", "_context": context.Background()}, base, "fs=workspace")
	two := approvalResumeFingerprint("terminal", map[string]interface{}{"_context": context.TODO(), "command": "git status"}, base, "fs=workspace")
	if one == "" || one != two {
		t.Fatalf("internal transport args changed fingerprint: %q vs %q", one, two)
	}
	changedAction := approvalResumeFingerprint("terminal", map[string]interface{}{"command": "git push"}, base, "fs=workspace")
	changedPrincipal := base
	changedPrincipal.PrincipalFingerprint = "other"
	changedIdentity := approvalResumeFingerprint("terminal", map[string]interface{}{"command": "git status"}, changedPrincipal, "fs=workspace")
	if one == changedAction || one == changedIdentity {
		t.Fatalf("fingerprint failed to bind action/identity: base=%q action=%q identity=%q", one, changedAction, changedIdentity)
	}
}
