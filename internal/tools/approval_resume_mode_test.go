package tools

import (
	"context"
	"testing"
)

// A person's approval of a parked action is an authorization for exactly that
// action, keyed on its fingerprint and the Run lineage, not on the approval
// mode. Work that waits can be narrowed to a stricter mode by a tightening of
// /mode, and the action the person already approved must still go through
// under it, exactly once.
func TestParkedApprovalStillReleasesItsActionUnderEveryNarrowerMode(t *testing.T) {
	for _, mode := range []ApprovalMode{ApprovalSmart, ApprovalAutoEdit, ApprovalOnRequest, ApprovalReadOnly} {
		t.Run(string(mode), func(t *testing.T) {
			dir := canonicalPath(t, t.TempDir())
			tenant := "tenant-narrowed-" + string(mode)
			resume := &resumeByFingerprint{authorized: map[string]bool{}}
			var asks []ToolApprovalRequest
			t.Cleanup(SetExecutionScope(tenant, ExecutionScope{
				TenantID: tenant, PersonID: "person-narrowed", TaskID: "task-narrowed", RunID: "run-narrowed-" + string(mode),
				WorkspaceRoot: dir, AllowedRoots: []string{dir}, ApprovalMode: mode,
				ResumeAuthorizations: resume, StandingGrants: InteractiveStandingGrants(),
				IntentSnapshot: func() RunIntentSnapshot {
					return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "tidy the scripts"}
				},
				Approval: func(_ context.Context, request ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks = append(asks, request)
					return ToolApprovalDecision{Approved: false, ApprovalID: "apr-narrowed"}, nil
				},
			}))
			ran := 0
			exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran++; return "ran", nil })
			call := func() error {
				_, err := exec(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": "chmod 777 script.sh", "cwd": dir})
				return err
			}
			// The first attempt parks on the person.
			if err := call(); err == nil || len(asks) != 1 || ran != 0 {
				t.Fatalf("fixture: the action must park on a person under %s: asks=%d ran=%d err=%v", mode, len(asks), ran, err)
			}
			// They approve it; the regenerated action is the same action.
			resume.authorized[asks[0].AuthorizationFingerprint] = true
			if err := call(); err != nil || ran != 1 || len(asks) != 1 {
				t.Fatalf("the approved action must be released without asking again under %s: asks=%d ran=%d err=%v", mode, len(asks), ran, err)
			}
			// The authorization is spent: the same action asks again.
			if err := call(); err == nil || ran != 1 || len(asks) != 2 {
				t.Fatalf("a spent authorization released a second call under %s: asks=%d ran=%d err=%v", mode, len(asks), ran, err)
			}
		})
	}
}
