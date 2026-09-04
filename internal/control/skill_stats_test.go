package control

import (
	"context"
	"testing"
)

func TestSkillUsageStatsDeriveFromActivationsAndWorkUnitOutcomes(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "skill-stats", "Skill Stats")
	if err != nil {
		t.Fatal(err)
	}

	runCase := func(title, runStatus, verification string, explicitFallback bool) {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			Title: title, Channel: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
			IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
			PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
			SkillKey: "skill-stats-key", SkillName: "stats-flow", VersionHash: "version-1",
			ActivationSource: "model", ContentBody: "## Procedure\nInspect.", CreatedBy: "test",
		}); err != nil {
			t.Fatal(err)
		}
		if explicitFallback {
			if _, err := store.FallbackCurrentSkill(ctx, SkillFallbackInput{
				IdentityTenantID: identity.TenantID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
				Reason: "procedure was not applicable", ErrorCategory: "environment_mismatch",
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: runStatus, TaskID: task.ID,
			TaskStatus: runStatus, Summary: title, VerificationState: verification,
			Channel: "cli", Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	runCase("completed", "done", "passed", false)
	runCase("fallback recovered", "done", "passed", true)
	runCase("failed", "failed", "failed", false)

	stats, err := store.SkillUsageStats(ctx, identity.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	got := stats[0]
	if got.SkillName != "stats-flow" || got.Calls != 3 || got.Completed != 1 || got.Fallbacks != 1 || got.Failures != 1 || got.Parked != 0 {
		t.Fatalf("canonical durable stats=%+v", got)
	}
}
