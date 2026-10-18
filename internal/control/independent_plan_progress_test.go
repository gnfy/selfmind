package control

import (
	"context"
	"testing"
)

func TestInitialVerificationObligationDoesNotReorderIndependentWork(t *testing.T) {
	for _, version := range []int{3, 4} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			ctx := context.Background()
			s, owner, _, run := newRecoveryFixture(t)
			if _, err := s.db.ExecContext(ctx, `UPDATE runs SET recovery_contract_version=? WHERE id=?`, version, run.ID); err != nil {
				t.Fatal(err)
			}
			plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "independent investigation", []RunPlanStepInput{
				{Step: "Inspect remote state", Status: "completed", SuccessCriteria: "remote state was checked", VerificationRequired: true},
				{Step: "Read independent records", Status: "completed"},
				{Step: "Prepare report", Status: "in_progress"},
			})
			if err != nil {
				t.Fatal(err)
			}
			steps := plan.Plan.Steps
			if steps[1].Status != "completed" {
				t.Fatal("independent completed work was erased")
			}
			if version == 4 {
				if steps[0].Status != "pending" || steps[2].Status != "in_progress" {
					t.Fatalf("Main's active independent work was reordered: %+v", steps)
				}
			} else if steps[0].Status != "in_progress" || steps[2].Status != "pending" {
				t.Fatal("historical contract changed")
			}
			if s.ValidateRunCompletion(ctx, owner.TenantID, run.ID) == nil {
				t.Fatal("missing required verification became completion")
			}
		})
	}
}
