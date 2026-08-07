package control

import (
	"context"
	"testing"
)

func TestTaskBlockerAuditOnlyBackfillsExactLatestRunEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "audit-user", "Audit User")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(title, runStatus, taskStatus string) (*Task, *Run) {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, runStatus); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, taskStatus, title, nil); err != nil {
			t.Fatal(err)
		}
		return task, run
	}
	safeTask, safeRun := seed("needs an answer", "waiting_user", "waiting_user")
	mixedTask, _ := seed("mixed historical label", "done", "interrupted")

	findings, err := store.AuditMissingTaskBlockers(ctx, identity.TenantID, 20)
	if err != nil {
		t.Fatal(err)
	}
	byTask := map[string]TaskBlockerAuditFinding{}
	for _, finding := range findings {
		byTask[finding.TaskID] = finding
	}
	if finding := byTask[safeTask.ID]; !finding.SafeToApply || finding.LatestRunID != safeRun.ID || finding.BlockerKind != "waiting_user" {
		t.Fatalf("safe finding = %+v", finding)
	}
	if finding := byTask[mixedTask.ID]; finding.SafeToApply || finding.Reason == "" {
		t.Fatalf("mixed history must remain review-only: %+v", finding)
	}
	changed, err := store.BackfillTaskBlocker(ctx, identity.TenantID, byTask[safeTask.ID])
	if err != nil || !changed {
		t.Fatalf("backfill changed=%v err=%v", changed, err)
	}
	if changed, err := store.BackfillTaskBlocker(ctx, identity.TenantID, byTask[safeTask.ID]); err != nil || changed {
		t.Fatalf("backfill replay changed=%v err=%v", changed, err)
	}
	blockers, err := store.ListOpenTaskBlockers(ctx, identity.TenantID, safeTask.ID, 10)
	if err != nil || len(blockers) != 1 || blockers[0].OriginRunID != safeRun.ID {
		t.Fatalf("blockers=%+v err=%v", blockers, err)
	}
	if blockers, err := store.ListOpenTaskBlockers(ctx, identity.TenantID, mixedTask.ID, 10); err != nil || len(blockers) != 0 {
		t.Fatalf("review-only task changed: %+v err=%v", blockers, err)
	}
}
