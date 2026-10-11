package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel"
)

func TestWorkInspectPagesAllPlanEvidenceWithoutChangingAuthority(t *testing.T) {
	ctx := context.Background()
	s := controltest.NewStore(t)
	owner, err := s.ResolveOrCreateAccount(ctx, "default", "cli", "page-user", "User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "16 conditions", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.StartRun(ctx, task, "cli", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	var steps []control.RunPlanStepInput
	for i := 0; i < 16; i++ {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		steps = append(steps, control.RunPlanStepInput{Step: fmt.Sprintf("Check target %d", i), Status: status, SuccessCriteria: "ready", VerificationRequired: true})
	}
	if _, err := s.SyncRunPlan(ctx, owner.TenantID, parent.ID, "check", steps); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, owner.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := s.StartRunWithOptions(ctx, task, "cli", "continue", control.StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewWorkInspectTool(s)
	for _, offset := range []int{0, 12} {
		out, err := tool.Execute(map[string]any{"run_id": child.ID, "plan_offset": offset,
			"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: owner.TenantID, PersonID: owner.PersonID, RunID: child.ID}})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Plan struct {
				Steps []control.RunPlanStep `json:"steps"`
			} `json:"plan"`
			Page map[string]int                  `json:"plan_page"`
			Refs []control.InheritedPlanEvidence `json:"source_step_evidence"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		want := min(12, 16-offset)
		if len(result.Plan.Steps) != want || len(result.Refs) != want || result.Page["total_steps"] != 16 {
			t.Fatalf("offset=%d: %s", offset, out)
		}
		for i, ref := range result.Refs {
			if ref.StepID != result.Plan.Steps[i].StepID || ref.SourceRunID != parent.ID {
				t.Fatalf("wrong provenance: %+v", ref)
			}
		}
	}
	if err := s.ValidateRunCompletion(ctx, owner.TenantID, child.ID); err == nil {
		t.Fatal("inspection granted completion authority")
	}
}
