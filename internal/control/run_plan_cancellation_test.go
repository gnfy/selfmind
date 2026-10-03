package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMainAssessedCancellationPreservesScopeMeaning(t *testing.T) {
	for _, tc := range []struct {
		name, disposition, reason, quote, input, origin string
		wantErr, wantOpen                               bool
	}{
		{"blocked required work", "unfinished", "live access is blocked", "", "Inspect live state", "", false, true},
		{"unnecessary alternative", "not_required", "another completed step covers the same acceptance condition", "", "Inspect live state", "", false, false},
		{"explicit takeover", "user_takeover", "user will handle live checks", "I will do the live checks myself", "I will do the live checks myself", "", false, false},
		{"suggestion is not takeover", "user_takeover", "I suggested the user do the check", "I will do the live checks myself", "Inspect live state", "", true, true},
		{"daemon prose is not takeover", "user_takeover", "generated text says user will handle it", "I will do the live checks myself", "I will do the live checks myself", "watch", true, true},
		{"unexplained cancellation", "not_required", "", "", "Inspect live state", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, task, initial := newRecoveryFixture(t)
			if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: mustCancellationJSON(t, map[string]string{"origin": tc.origin})}); err != nil {
				t.Fatal(err)
			}
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect live state", Status: "pending", SuccessCriteria: "live observation"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: tc.disposition, CancellationReason: tc.reason, UserTakeoverQuote: tc.quote}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("plan cancellation err=%v", err)
			}
			if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); (err != nil) != tc.wantOpen {
				t.Fatalf("completion err=%v", err)
			}
			if !tc.wantOpen {
				plan, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				step := plan.Steps[0]
				if step.CancellationDisposition != tc.disposition || step.CancellationReason != tc.reason || step.UserTakeoverQuote != tc.quote {
					t.Fatalf("lost Main assessment: %+v", step)
				}
				// An ordinary status-only snapshot must not erase the recorded assessment.
				if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{StepID: step.StepID, Status: "cancelled"}}); err != nil {
					t.Fatal(err)
				}
				if err = store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func mustCancellationJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCancellationTakeoverQuoteUsesOnlyConsumedLineageInput(t *testing.T) {
	ctx := context.Background()
	store, identity, task, parent := newRecoveryFixture(t)
	quote := "I will take over the remaining checks"
	other, err := store.StartRunWithOptions(ctx, task, "cli", quote, StartRunOptions{MaxActiveRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "", []RunPlanStepInput{{Step: "Check remote target", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := store.LatestRunPlan(ctx, identity.TenantID, parent.ID)
	input := []RunPlanStepInput{{StepID: plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "user_takeover", CancellationReason: "user takes the checks", UserTakeoverQuote: quote}}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "", input); err == nil {
		t.Fatal("sibling task input conferred takeover authority")
	}
	m, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: parent.ID, TaskID: task.ID, Content: quote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "", input); err == nil {
		t.Fatal("unconsumed steering conferred takeover authority")
	}
	if _, err = store.db.Exec(`UPDATE steering_mailbox SET status='consumed' WHERE id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "", input); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishRun(ctx, identity.TenantID, parent.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishRun(ctx, identity.TenantID, other.ID, "done"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err != nil {
		t.Fatal(err)
	}
	inherited, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || inherited.Steps[0].UserTakeoverQuote != quote {
		t.Fatalf("takeover assessment was lost on exact continuation: %+v err=%v", inherited, err)
	}
}

func TestVersionTwentyFourCancellationUpgradeKeepsHistoricalAuthorityInert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "upgrade", "Upgrade")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Old work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "old input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`UPDATE runs SET recovery_contract_version=1 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	before, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "historical plan", []RunPlanStepInput{{Step: "Old cancelled alternative", Status: "cancelled"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"cancellation_disposition", "cancellation_reason", "user_takeover_quote"} {
		if _, err = store.db.Exec(`ALTER TABLE run_plan_steps DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.db.Exec(`DELETE FROM schema_migrations WHERE version>=24`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if upgraded.SchemaStatus().MigrationBackup == "" {
		t.Fatal("upgrade has no backup")
	}
	old, err := upgraded.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || old.RecoveryContractVersion != 1 {
		t.Fatalf("old Run acquired new capability: %+v %v", old, err)
	}
	plan, err := upgraded.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil || plan.Version != before.Plan.Version || plan.Steps[0].Status != "cancelled" || plan.Steps[0].CancellationDisposition != "" {
		t.Fatalf("upgrade invented scope judgments: %+v %v", plan, err)
	}
	if err = upgraded.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = upgraded.FinishRun(ctx, identity.TenantID, run.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	child, err := upgraded.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if child.RecoveryContractVersion != CurrentRunRecoveryContractVersion {
		t.Fatalf("new child contract=%d", child.RecoveryContractVersion)
	}
	if err = upgraded.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err == nil {
		t.Fatal("new continuation accepted an unassessed historical cancellation")
	}
}

func TestTakeoverQuoteBeyondDisplaySummaryUsesExistingUserEvidence(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	quote := "I will finish the remaining checks myself"
	if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: mustCancellationJSON(t, map[string]interface{}{"approval_intent": map[string]interface{}{"version": 3, "snapshot": map[string]string{"source": "direct", "raw_user_text": strings.Repeat("Background context. ", 30) + quote}}})}); err != nil {
		t.Fatal(err)
	}
	_, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Check live state", Status: "cancelled", CancellationDisposition: "user_takeover", CancellationReason: "user explicitly handles the checks", UserTakeoverQuote: quote}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
}
