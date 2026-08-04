package tools

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// TestSandboxContainedExecSkipsAskInSmartMode is batch C1's core claim: an exec
// call the sandbox can fully contain (isolated, workspace-writable, no network) is
// no more dangerous than the file writes smart mode already performs unprompted,
// so asking about it was fatigue with no safety return.
func TestSandboxContainedExecSkipsAskInSmartMode(t *testing.T) {
	if runtime.GOOS != "linux" || !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, false)
	asked := 0
	cleanup := SetExecutionScope("person-contain", ExecutionScope{
		TenantID: "tenant-contain", PersonID: "person-contain", TaskID: "task-contain",
		WorkspaceRoot: "/workspace/app", ApprovalMode: ApprovalSmart, Grants: newFakeGrantStore(),
		Judge: &fakeJudge{reply: "APPROVE"},
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr_contain"}, nil
		},
	})
	defer cleanup()
	resetTriageTelemetryForTest(t)

	ran := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-contain", "_tool_name": "terminal",
		"command": "go test ./internal/tools/", "sandbox": string(SandboxIsolated),
	}); err != nil {
		t.Fatalf("contained exec should run: %v", err)
	}
	if !ran {
		t.Fatal("command should have executed")
	}
	if asked != 0 {
		t.Fatalf("a contained call must not ask, asked %d times", asked)
	}
	if stats := TriageDiagnostics("tenant-contain", "person-contain"); stats.Contained != 1 {
		t.Fatalf("containment should be counted for /diag, stats = %+v", stats)
	}
}

// TestContainmentNeverCoversEscapesOrOtherModes pins the boundaries. Containment is
// a claim about blast radius, so it evaporates the moment the call can reach
// outside the sandbox — and it is smart-mode only, because on-request and
// read-only are literal contracts.
func TestContainmentNeverCoversEscapesOrOtherModes(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	cases := []struct {
		name string
		args map[string]interface{}
		mode ApprovalMode
	}{
		{
			name: "host mode is not contained",
			args: map[string]interface{}{"_tool_name": "terminal", "command": "ls", "sandbox": string(SandboxHost)},
			mode: ApprovalSmart,
		},
		{
			name: "egress is dangerous even when isolated",
			args: map[string]interface{}{"_tool_name": "terminal", "command": "curl -sSL https://example.com/x", "sandbox": string(SandboxIsolated)},
			mode: ApprovalSmart,
		},
		{
			name: "read-only keeps its literal contract",
			args: map[string]interface{}{"_tool_name": "terminal", "command": "ls", "sandbox": string(SandboxIsolated)},
			mode: ApprovalReadOnly,
		},
		{
			name: "on-request keeps its literal contract",
			args: map[string]interface{}{"_tool_name": "terminal", "command": "ls", "sandbox": string(SandboxIsolated)},
			mode: ApprovalOnRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			cleanup := SetExecutionScope("person-escape", ExecutionScope{
				TenantID: "tenant-escape", PersonID: "person-escape", TaskID: "task-escape",
				WorkspaceRoot: "/workspace/app", ApprovalMode: tc.mode, Grants: newFakeGrantStore(),
				Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					asked++
					return ToolApprovalDecision{Approved: true, ApprovalID: "apr_escape"}, nil
				},
			})
			defer cleanup()
			exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ok", nil })
			args := map[string]interface{}{"_tenant_id": "person-escape"}
			for k, v := range tc.args {
				args[k] = v
			}
			if _, err := exec(args); err != nil {
				t.Fatalf("approved call should run: %v", err)
			}
			if asked != 1 {
				t.Fatalf("this call must still reach a decision surface, asked %d times", asked)
			}
		})
	}
}

// TestNetworkPolicyDefeatsContainment: the sandbox's guarantee is
// "workspace-writable, NO network". With egress enabled for the workspace, a
// contained write becomes a potential exfiltration, so the claim must not hold.
func TestNetworkPolicyDefeatsContainment(t *testing.T) {
	withExecSandboxPolicy(t, true, true, true)
	args := map[string]interface{}{"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated)}
	if execSandboxContained("terminal", args) {
		t.Fatal("network-enabled policy must not claim containment")
	}
}

func TestThreeAxisContainmentReleasesOnlyDeclaredObservations(t *testing.T) {
	if runtime.GOOS != "linux" || !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, true)
	read := map[string]interface{}{
		"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
		"_network_shared": true, credentialReadArgKey: true,
		"command": "gcloud builds describe build-123",
	}
	assessment := assessExecContainment("terminal", read)
	if assessment.Filesystem != containmentFilesystemIsolated || assessment.Network != containmentNetworkShared || assessment.Credentials != containmentCredentialsSelected {
		t.Fatalf("three axes lost: %+v", assessment)
	}
	if !assessment.ObservationOnly || !assessment.AutoApprove() {
		t.Fatalf("declared observation should be contained: %+v", assessment)
	}

	for name, args := range map[string]map[string]interface{}{
		"arbitrary code": {
			"_tool_name": "execute_code", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, credentialReadArgKey: true, "code": "print('x')",
		},
		"sensitive kubernetes read": {
			"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, credentialReadArgKey: true, "command": "kubectl get secrets -A",
		},
		"unknown agent cli": {
			"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, credentialReadArgKey: true, "command": "future-agent inspect repo",
		},
		"generic reader with credentials": {
			"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, credentialReadArgKey: true, "command": "cat ~/.config/gcloud/application_default_credentials.json",
		},
		"rg preprocessor": {
			"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, "command": "rg --pre ./helper pattern .",
		},
		"in-place jq": {
			"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
			"_network_shared": true, "command": "jq -i . config.json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := assessExecContainment(stringArg(args, "_tool_name"), args); got.AutoApprove() {
				t.Fatalf("must remain approval-gated: %+v", got)
			}
		})
	}
}

// TestContainmentRequiresExplicitEffectiveMode protects the /mode retro path: it
// re-judges a PENDING approval from redacted args, where a host-mode call's marker
// may be gone. Inferring "isolated" there would auto-approve a host escape.
func TestContainmentRequiresExplicitEffectiveMode(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	if execSandboxContained("terminal", map[string]interface{}{"_tool_name": "terminal", "command": "ls"}) {
		t.Fatal("containment must require the explicit effective-mode annotation")
	}
	if EvaluateModeDecision(context.Background(), ApprovalSmart, "", "terminal",
		map[string]interface{}{"command": "ls"}, "", false, &fakeJudge{err: errors.New("down")}) == ModeApprove {
		t.Fatal("a pending approval must never be released by an inferred containment")
	}
}

// TestGuardianAssessmentParsing pins the C2 contract: structured JSON is read,
// a legacy one-word reply still works, and everything else escalates.
func TestGuardianAssessmentParsing(t *testing.T) {
	verdict, assessment := parseTriageAssessment(`{"risk_level":"low","user_authorization":"high","outcome":"approve","rationale":"The person asked for a build."}`)
	if verdict != TriageApprove {
		t.Fatalf("verdict = %v, want approve", verdict)
	}
	if assessment.Risk != "low" || assessment.Authorization != "high" {
		t.Fatalf("assessment axes lost: %+v", assessment)
	}
	if !strings.Contains(assessment.Rationale, "asked for a build") {
		t.Fatalf("rationale = %q", assessment.Rationale)
	}

	// Wrapped in prose or a fence: still read.
	if v, a := parseTriageAssessment("Here you go:\n```json\n{\"outcome\":\"deny\",\"risk_level\":\"critical\",\"rationale\":\"Destroys the disk.\"}\n```"); v != TriageDeny || a.Risk != "critical" {
		t.Fatalf("fenced JSON should parse: %v %+v", v, a)
	}
	// Legacy one-word replies keep working.
	if v, _ := parseTriageAssessment("APPROVE"); v != TriageApprove {
		t.Fatal("legacy APPROVE must still parse")
	}
	// Fail safe: unknown outcome, unparseable text, and out-of-contract levels.
	if v, _ := parseTriageAssessment(`{"outcome":"maybe"}`); v != TriageEscalate {
		t.Fatal("an unknown outcome must escalate")
	}
	if v, _ := parseTriageAssessment("I think it's probably fine?"); v != TriageEscalate {
		t.Fatal("prose must escalate")
	}
	if _, a := parseTriageAssessment(`{"outcome":"approve","risk_level":"extremely spicy"}`); a.Risk != "" {
		t.Fatalf("out-of-contract risk must be dropped, got %q", a.Risk)
	}
}

// TestTriageDenyBreakerHandsOffToHuman: a model circling a denied action must reach
// the person, who is the only one who can end the loop. The breaker never approves.
func TestTriageDenyBreakerHandsOffToHuman(t *testing.T) {
	resetTriageTelemetryForTest(t)
	clearTriageDenials("run-breaker")
	judge := &fakeJudge{reply: `{"outcome":"deny","risk_level":"high","rationale":"Deletes production data."}`}
	asked := 0
	cleanup := SetExecutionScope("person-breaker", ExecutionScope{
		TenantID: "tenant-breaker", PersonID: "person-breaker", TaskID: "task-breaker", RunID: "run-breaker",
		ApprovalMode: ApprovalSmart, Grants: newFakeGrantStore(), Judge: judge,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			return ToolApprovalDecision{Approved: false, ApprovalID: "apr_breaker", Outcome: ApprovalOutcomeDenied, Reason: "no"}, nil
		},
	})
	defer cleanup()
	defer clearTriageDenials("run-breaker")

	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ok", nil })
	call := func() error {
		_, err := exec(map[string]interface{}{
			"_tenant_id": "person-breaker", "_tool_name": "terminal", "command": "rm -rf /var/data/prod",
		})
		return err
	}
	// First denials come from the judge and carry its rationale.
	if err := call(); err == nil || !strings.Contains(err.Error(), "Deletes production data") {
		t.Fatalf("judge denial should carry the rationale, got %v", err)
	}
	if err := call(); err == nil {
		t.Fatal("second denial should also be blocked")
	}
	if asked != 0 {
		t.Fatalf("judge denials must not reach the human, asked %d", asked)
	}
	// Breaker tripped: the next dangerous op goes to the person instead.
	if err := call(); err == nil {
		t.Fatal("the human refusal should still block the call")
	}
	if asked != 1 {
		t.Fatalf("after the breaker trips the person must be asked exactly once, asked %d", asked)
	}
	if judge.calls != 2 {
		t.Fatalf("a tripped breaker must stop spending judge calls, calls = %d", judge.calls)
	}
}
