package httpapi

import (
	"context"
	"encoding/json"
	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/verification"
	"strings"
	"testing"
)

func TestAdoptedVerificationAppearsInRunOutcome(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ordinary     bool
		mutation     string
		failedCheck  bool
		skipAdoption bool
		want         string
	}{
		{name: "no_child_command", want: "passed"},
		{name: "readonly_child_command", ordinary: true, want: "passed"},
		{name: "independent_report", mutation: "/workspace/report.md", want: "passed"},
		{name: "changed_declared_input", mutation: "/workspace/input.json", want: "blocked"},
		{name: "current_independent_failure", failedCheck: true, want: "failed"},
		{name: "changed_input_and_independent_failure", mutation: "/workspace/input.json", failedCheck: true, want: "failed"},
		{name: "historical_pass_without_main_adoption", skipAdoption: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, store, identity := newTaskViewServer(t)
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Check input", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.StartRun(ctx, task, "cli", "check input")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "check", []control.RunPlanStepInput{{Step: "Check input", Status: "in_progress", SuccessCriteria: "input is ready", VerificationRequired: true}})
			if err != nil {
				t.Fatal(err)
			}
			id := plan.Plan.Steps[0].StepID
			deps := []string{"/workspace/input.json"}
			binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, parent.ID, verification.Binding{StepID: id, Target: "input.json", LocalDependencies: &deps}, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			started, exitCode := true, 0
			check := kernel.RunEvidence{Invoked: &started, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exitCode, NetworkDecision: "withheld: test isolation", NetworkPermission: "trust"}, ToolCallID: "source-check", ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 10, FinishedAt: 11, Command: &kernel.CommandEvidence{Command: "go test ./...", CWD: "/workspace", Binding: binding}}
			raw, err := json.Marshal(map[string]interface{}{"evidence": check})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.AppendEvent(ctx, control.Event{RunID: parent.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if _, err = store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "checked", []control.RunPlanStepInput{{StepID: id, Status: "completed"}}); err != nil {
				t.Fatal(err)
			}
			if err = store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", control.StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			if tc.ordinary {
				started := true
				exit := 0
				command := kernel.RunEvidence{ToolCallID: "observe-input", ToolName: "terminal", Kind: "command", Status: "succeeded", StartedAt: 20, FinishedAt: 21, Invoked: &started, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit}, Command: &kernel.CommandEvidence{Command: "stat input.json", CWD: "/workspace"}}
				raw, err = json.Marshal(map[string]interface{}{"evidence": command})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.AppendEvent(ctx, control.Event{RunID: child.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}

			if _, err = store.EnsureInheritedRunPlan(ctx, identity.TenantID, child.ID); err != nil {
				t.Fatal(err)
			}
			if !tc.skipAdoption {
				adopted := []control.RunPlanStepInput{{StepID: id, Status: "completed", ReusePriorVerification: true, ReuseReason: "Same target and unchanged declared input."}}
				accepted, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "adopt", adopted)
				if err != nil {
					t.Fatal(err)
				}
				if len(accepted.WorkUnits) != 1 || accepted.WorkUnits[0].VerificationState != "passed" {
					t.Fatalf("adoption did not pass: %+v", accepted.WorkUnits)
				}
			}
			if tc.mutation != "" {
				mutation := kernel.RunEvidence{ToolCallID: "new-write", ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 30, FinishedAt: 31, Files: []kernel.FileEffect{{Path: tc.mutation, BeforeSHA256: "before", AfterSHA256: "after"}}}
				if _, err = store.AppendEvent(ctx, control.Event{RunID: child.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": mutation})}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failedCheck {
				// Reusing the source's call id in another Run cannot replace its proof.
				failed := kernel.RunEvidence{ToolCallID: "source-check", ToolName: "verify", Kind: "verification", Status: "failed", StartedAt: 40, FinishedAt: 41, Command: &kernel.CommandEvidence{Kind: "custom", Command: "check independent target", CWD: "/workspace", ExitCode: 1}}
				if _, err = store.AppendEvent(ctx, control.Event{RunID: child.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": failed})}); err != nil {
					t.Fatal(err)
				}
			}
			result, _ := srv.coordinator().evidenceOutcome(ctx, identity.TenantID, child.ID)
			if tc.skipAdoption {
				if result != nil && result.State == "passed" {
					t.Fatalf("historical pass became automatic adoption: %+v", result)
				}
				return
			}
			if result == nil || result.State != tc.want {
				t.Fatalf("verification outcome = %+v, want %s", result, tc.want)
			}
			if tc.want != "blocked" && tc.mutation != "/workspace/input.json" {
				adopted := result.Checks[0]
				if adopted.Source == nil || adopted.Source.RunID != parent.ID || adopted.Source.StepID != id || adopted.Source.PlanVersion < 1 || adopted.Source.Reason == "" {
					t.Fatalf("source provenance lost: %+v", adopted)
				}
				if adopted.ToolCallID != "source-check" || adopted.StartedAt != 10 || adopted.FinishedAt != 11 || adopted.NetworkPermission != "trust" || adopted.ProcessStarted == nil || !*adopted.ProcessStarted {
					t.Fatalf("source observation facts changed: %+v", adopted)
				}
				if !strings.Contains(result.Summary, "adopted") {
					t.Fatalf("adoption not distinguished from a new execution: %s", result.Summary)
				}
			}
			if tc.ordinary && result.OrdinaryCommands != 1 {
				t.Fatalf("ordinary query count changed: %+v", result)
			}
			events, err := store.ListRunEvidenceEvents(ctx, identity.TenantID, child.ID, 0, 200)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if strings.Contains(string(event.Payload), `"kind":"verification"`) && !tc.failedCheck {
					t.Fatal("projection fabricated a child verification execution")
				}
			}
		})
	}
}
