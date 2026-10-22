package httpapi

import (
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// A recovery child only ever narrows what its interrupted parent was admitted
// with. Verification-only recovery drops the writers and still never offers a
// tool the parent did not: a parent that had no planning tool does not gain
// one, and one that had to plan is released from the obligation, not from its
// other limits. A parent that was already verification-only never widens back
// when a later recovery asks only to continue.
func TestRecoveryAdmissionNeverWidensItsParent(t *testing.T) {
	probes := []string{"update_plan", "web_search", "web_extract", "terminal", "read_file", "finish_run", "ask_user"}
	for _, plan := range []kernel.PlanPolicy{kernel.PlanPolicyDisabled, kernel.PlanPolicyOptional, kernel.PlanPolicyRequired} {
		for _, web := range []kernel.WebPolicy{kernel.WebPolicyDisabled, kernel.WebPolicyEnabled} {
			for _, toolMode := range []kernel.ToolMode{kernel.ToolModeNone, kernel.ToolModeLocalRead, kernel.ToolModeFull} {
				parent := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "smart",
					Strategy: control.AdmissionStrategy{Class: "general_task", ToolMode: string(toolMode), PlanPolicy: string(plan),
						WebPolicy: string(web), ChannelMode: "interactive"}}
				parentStrategy := admittedStrategy(parent)
				for _, mode := range []string{control.RunRecoveryModeContinue, control.RunRecoveryModeVerifyOnly} {
					child := recoveryAdmission(parent, mode)
					childStrategy := admittedStrategy(child)
					name := string(plan) + "/" + string(web) + "/" + string(toolMode) + "/" + mode
					for _, tool := range probes {
						if childStrategy.AllowsTool(tool) && !parentStrategy.AllowsTool(tool) {
							t.Fatalf("%s: the recovery child offers %s, which its parent did not", name, tool)
						}
					}
					if child.RecoveryMode != mode {
						t.Fatalf("%s: recovery mode = %q", name, child.RecoveryMode)
					}
					if mode == control.RunRecoveryModeVerifyOnly {
						if !child.Strategy.VerificationOnly {
							t.Fatalf("%s: a verification-only recovery must be verification-only", name)
						}
						if toolMode != kernel.ToolModeNone && child.Strategy.ToolMode != string(kernel.ToolModeLocalRead) {
							t.Fatalf("%s: tool mode = %q, want the read-only surface", name, child.Strategy.ToolMode)
						}
						if toolMode == kernel.ToolModeNone && child.Strategy.ToolMode != string(kernel.ToolModeNone) {
							t.Fatalf("%s: a parent with no tools gained %q", name, child.Strategy.ToolMode)
						}
						want := string(plan)
						if plan == kernel.PlanPolicyRequired {
							want = string(kernel.PlanPolicyOptional)
						}
						if child.Strategy.PlanPolicy != want {
							t.Fatalf("%s: plan policy = %q, want %q", name, child.Strategy.PlanPolicy, want)
						}
					} else if child.Strategy != parent.Strategy {
						t.Fatalf("%s: a continue recovery changed the strategy: %+v", name, child.Strategy)
					}
				}
			}
		}
	}
}

// The guard against widening back: a parent that was verification-only keeps
// its read-only surface even when the recovery that continues it is not.
func TestRecoveryOfAVerificationOnlyParentStaysVerificationOnly(t *testing.T) {
	parent := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "smart",
		Strategy: control.AdmissionStrategy{Class: "general_task", ToolMode: string(kernel.ToolModeLocalRead), PlanPolicy: string(kernel.PlanPolicyOptional),
			WebPolicy: string(kernel.WebPolicyDisabled), ChannelMode: "interactive", VerificationOnly: true},
		RecoveryMode: control.RunRecoveryModeVerifyOnly}
	child := recoveryAdmission(parent, control.RunRecoveryModeContinue)
	if child.RecoveryMode != control.RunRecoveryModeVerifyOnly || !child.Strategy.VerificationOnly ||
		child.Strategy.ToolMode != string(kernel.ToolModeLocalRead) {
		t.Fatalf("a continue recovery widened a verification-only parent back: %+v", child)
	}
	// The constraint that must change the result: a parent that was not
	// verification-only continues as it was.
	parent.RecoveryMode, parent.Strategy.VerificationOnly, parent.Strategy.ToolMode = "", false, string(kernel.ToolModeFull)
	if child := recoveryAdmission(parent, control.RunRecoveryModeContinue); child.RecoveryMode != control.RunRecoveryModeContinue ||
		child.Strategy.VerificationOnly || child.Strategy.ToolMode != string(kernel.ToolModeFull) {
		t.Fatalf("a continue recovery narrowed an ordinary parent: %+v", child)
	}
}
