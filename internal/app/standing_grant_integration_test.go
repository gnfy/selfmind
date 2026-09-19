package app

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// The whole point of a standing class, proved end to end against a real grant
// store: a person answers once, and the SAME operation stops asking — including
// tomorrow, in a new run, with different arguments. A run-scoped reuse died
// with the run, which is why one week of real traffic re-answered 478 approvals
// across 56 runs for 121 distinct classes.
//
// The same test pins what it must NOT release: a different operation of the
// same program, and the same operation in another workspace.
func TestStandingClassStopsAskingForTheSameOperation(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	const person = "person_standing"
	asks := 0
	newScope := func(workspaceID string) tools.ExecutionScope {
		return tools.ExecutionScope{
			TenantID: control.DefaultTenantID, PersonID: person,
			WorkspaceID: workspaceID, WorkspaceRoot: "/work/" + workspaceID,
			ApprovalMode: tools.ApprovalOnRequest, Grants: store,
			StandingGrants: tools.InteractiveStandingGrants(),
			Approval: func(context.Context, tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
				asks++
				// The person picks the standing answer the daemon offered.
				return tools.ToolApprovalDecision{Approved: true, ApprovalID: "apr", Scope: "workspace"}, nil
			},
		}
	}

	run := func(workspaceID, command string) bool {
		ran := false
		cleanup := tools.SetExecutionScope(person, newScope(workspaceID))
		defer cleanup()
		exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
			ran = true
			return "ok", nil
		})
		_, err := exec(map[string]interface{}{
			"_tenant_id": person, "_tool_name": "terminal",
			"command": command, "sandbox": "host", "_effective_sandbox_mode": string(tools.SandboxHost),
		})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return ran
	}

	// First invocation asks, and the person answers with the standing scope.
	if !run("ws-a", "aws codebuild batch-get-builds --ids build-one --profile cw3") || asks != 1 {
		t.Fatalf("first invocation: asks=%d", asks)
	}

	// Same operation, different arguments: the build id changes on every call,
	// so a class that included it would have needed a new answer here.
	if !run("ws-a", "aws codebuild batch-get-builds --ids build-two --profile cw3") {
		t.Fatal("second invocation did not run")
	}
	if asks != 1 {
		t.Fatalf("the standing class did not release the same operation: asks=%d", asks)
	}

	// A different operation of the same program must still ask: stopping the
	// prefix at the program would have let a remembered read release a write.
	if !run("ws-a", "aws codebuild start-build --project-name p --profile cw3") {
		t.Fatal("write invocation did not run")
	}
	if asks != 2 {
		t.Fatalf("a write was released by a read's class: asks=%d", asks)
	}

	// Another workspace must still ask: a standing class is scoped to the
	// workspace that minted it.
	if !run("ws-b", "aws codebuild batch-get-builds --ids build-one --profile cw3") {
		t.Fatal("other-workspace invocation did not run")
	}
	if asks != 3 {
		t.Fatalf("a standing class leaked across workspaces: asks=%d", asks)
	}
}

// Scheduled work may USE a standing class but is frozen to the classes that
// existed when the person authorised the schedule. A job created before a class
// was granted must not silently gain it.
func TestScheduledWorkDoesNotInheritLaterClasses(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	const person = "person_sched"
	const workspace = "ws-sched"
	command := "aws codebuild batch-get-builds --ids b --profile cw3"

	asks := 0
	runWith := func(policy tools.StandingGrantPolicy, answer string) bool {
		ran := false
		scope := tools.ExecutionScope{
			TenantID: control.DefaultTenantID, PersonID: person,
			WorkspaceID: workspace, WorkspaceRoot: "/work/sched",
			ApprovalMode: tools.ApprovalOnRequest, Grants: store, StandingGrants: policy,
			Approval: func(context.Context, tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
				asks++
				return tools.ToolApprovalDecision{Approved: true, ApprovalID: "apr", Scope: answer}, nil
			},
		}
		cleanup := tools.SetExecutionScope(person, scope)
		defer cleanup()
		exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
			ran = true
			return "ok", nil
		})
		if _, err := exec(map[string]interface{}{
			"_tenant_id": person, "_tool_name": "terminal",
			"command": command, "sandbox": "host", "_effective_sandbox_mode": string(tools.SandboxHost),
		}); err != nil {
			t.Fatal(err)
		}
		return ran
	}

	// A person grants the class now.
	frozenBefore := time.Now().Add(-time.Minute)
	runWith(tools.InteractiveStandingGrants(), "workspace")
	if asks != 1 {
		t.Fatalf("setup: asks=%d", asks)
	}

	// A schedule authorised BEFORE that decision must still ask.
	runWith(tools.ScheduledStandingGrants(frozenBefore), "")
	if asks != 2 {
		t.Fatalf("a schedule inherited a class granted after it was authorised: asks=%d", asks)
	}

	// A schedule with no recorded authorisation consumes nothing.
	runWith(tools.StandingGrantPolicy{}, "")
	if asks != 3 {
		t.Fatalf("an unauthorised schedule consumed a standing class: asks=%d", asks)
	}
}
