package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/verification"
)

func TestAdoptedEvidenceStaysFreshUntilCompletion(t *testing.T) {
	for _, path := range []string{"", "/workspace/report.json", "/workspace/input.json"} {
		label := "unchanged"
		if path != "" {
			label = path
		}
		change := path == "/workspace/input.json"
		t.Run(label, func(t *testing.T) {
			ctx := context.Background()
			daemon, store, identity := newTaskViewServer(t)
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Check config", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.StartRun(ctx, task, "cli", "check config")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "check", []control.RunPlanStepInput{{Step: "Check input", Status: "in_progress", SuccessCriteria: "input is ready", VerificationRequired: true}})
			if err != nil {
				t.Fatal(err)
			}
			stepID := plan.Plan.Steps[0].StepID
			deps := []string{"/workspace/input.json"}
			b, err := store.ResolveVerificationBinding(ctx, identity.TenantID, parent.ID, verification.Binding{StepID: stepID, Target: "input.json", LocalDependencies: &deps}, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			check := kernel.RunEvidence{ToolCallID: "check", ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 10, FinishedAt: 11, Command: &kernel.CommandEvidence{Command: "check input", CWD: "/workspace", Binding: b}}
			raw, _ := json.Marshal(map[string]interface{}{"evidence": check})
			if _, err = store.AppendEvent(ctx, control.Event{RunID: parent.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if _, err = store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "checked", []control.RunPlanStepInput{{StepID: stepID, Status: "completed"}}); err != nil {
				t.Fatal(err)
			}
			if err = store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", control.StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			adopted := []control.RunPlanStepInput{{StepID: stepID, Status: "completed", ReusePriorVerification: true, ReuseReason: "The exact check still covers this unchanged input."}}
			accepted, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "adopt", adopted)
			if err != nil {
				t.Fatal(err)
			}
			if len(accepted.WorkUnits) != 1 {
				t.Fatalf("adopted units=%d", len(accepted.WorkUnits))
			}
			closedUnit := accepted.WorkUnits[0]
			if path != "" {
				mutation := kernel.RunEvidence{ToolCallID: "write", ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 20, FinishedAt: 21, Files: []kernel.FileEffect{{Path: path, BeforeSHA256: "before", AfterSHA256: "after"}}}
				raw, _ = json.Marshal(map[string]interface{}{"evidence": mutation})
				if _, err = store.AppendEvent(ctx, control.Event{RunID: child.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}
			// Finish must reject stale adoption even without another plan update.
			directErr := daemon.coordinator().newRunPlanProjection(identity, child, kernel.ToolInvocationScope{}).ValidateCompletion(ctx)
			if change && directErr == nil {
				t.Fatal("direct completion accepted changed dependencies")
			}
			if !change && directErr != nil {
				t.Fatal(directErr)
			}
			// Complete snapshot is the production path, with no new reuse judgment.
			_, syncErr := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "final status", adopted)
			completionErr := daemon.coordinator().newRunPlanProjection(identity, child, kernel.ToolInvocationScope{}).ValidateCompletion(ctx)
			if change && syncErr == nil {
				t.Fatal("plan update accepted stale adopted evidence")
			}
			if !change && syncErr != nil {
				t.Fatal(syncErr)
			}
			if change && completionErr == nil {
				t.Fatal("completion accepted after the adopted check's exact declared dependency changed")
			}
			if !change && completionErr != nil {
				t.Fatalf("unchanged evidence rejected: %v", completionErr)
			}
			if change {
				// Main reopens the obligation and supplies a fresh bound check;
				// stale proof does not force an irrecoverable close-out loop.
				reopened, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "recheck changed input", []control.RunPlanStepInput{{StepID: stepID, Status: "in_progress"}})
				if err != nil {
					t.Fatal(err)
				}
				if len(reopened.WorkUnits) != 2 || reopened.WorkUnits[0].ID == closedUnit.ID || reopened.WorkUnits[0].Status != control.WorkUnitActive {
					t.Fatal("reopened obligation did not get a fresh active evidence window")
				}
				old := reopened.WorkUnits[1]
				if old.ID != closedUnit.ID || old.Status != closedUnit.Status || old.PlanStatus != closedUnit.PlanStatus || old.StartedCursor != closedUnit.StartedCursor || old.FinishedCursor != closedUnit.FinishedCursor || string(old.VerificationRefs) != string(closedUnit.VerificationRefs) {
					t.Fatal("reopening changed the closed source evidence")
				}

				binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, child.ID, verification.Binding{StepID: stepID, Target: "input.json", LocalDependencies: &deps}, "/workspace")
				if err != nil {
					t.Fatal(err)
				}
				check.ToolCallID = "fresh-check"
				check.StartedAt = 30
				check.FinishedAt = 31
				check.Command.Binding = binding
				raw, _ = json.Marshal(map[string]interface{}{"evidence": check})
				if _, err = store.AppendEvent(ctx, control.Event{RunID: child.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
				if _, err = store.SyncRunPlan(ctx, identity.TenantID, child.ID, "fresh input verified", []control.RunPlanStepInput{{StepID: stepID, Status: "completed"}}); err != nil {
					t.Fatal(err)
				}
				if err = daemon.coordinator().newRunPlanProjection(identity, child, kernel.ToolInvocationScope{}).ValidateCompletion(ctx); err != nil {
					t.Fatalf("fresh proof did not restore completion: %v", err)
				}
			}
		})
	}
}
