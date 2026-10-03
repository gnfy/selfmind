package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

func TestLostResourceObservationBecomesActionableWithoutReplaying(t *testing.T) {
	daemon, store, owner, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	first, err := store.StartRunWithOptions(ctx, task, "cli", "first effect", control.StartRunOptions{MaxActiveRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "second", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.StartRunWithOptions(ctx, otherTask, "cli", "second effect", control.StartRunOptions{MaxActiveRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: first.ID, EffectID: "first-effect", TargetKeys: []string{"service:alpha"}})
	if err != nil || !claim.Granted {
		t.Fatalf("claim %+v %v", claim, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, first.ID, "first-effect"); err != nil {
		t.Fatal(err)
	}
	decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: second.ID, EffectID: "second-effect", TargetKeys: []string{"service:alpha"}})
	if err != nil || decision.NeedsObservation || decision.Granted {
		t.Fatalf("live owner wait %+v %v", decision, err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, second.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, first.ID, "done"); err != nil {
		t.Fatal(err)
	}
	daemon.runExternalResourceWaitPass(ctx)
	daemon.runExternalResourceWaitPass(ctx)
	got, err := store.GetRun(ctx, owner.TenantID, second.ID)
	if err != nil || got.Status != "blocked" {
		t.Fatalf("lost source still promised continuation: %+v %v", got, err)
	}
	claims, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 100)
	if err != nil || len(claims) != 1 || claims[0].State != control.ExternalClaimUncertain {
		t.Fatalf("uncertain effect was cleared %+v %v", claims, err)
	}
	outcome, ok := daemon.coordinator().latestStructuredRunOutcome(ctx, second.TaskID, second.ID)
	if !ok || outcome.CompletionReason != "external_effect_unresolved" || !outcome.Resumable {
		t.Fatalf("correction outcome: %+v", outcome)
	}
	row, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "external-resource:"+second.ID+":second-effect")
	if err != nil || row != nil {
		t.Fatalf("uncertain effect scheduled blind retry %+v %v", row, err)
	}
	notices, err := store.ListResourceObservationNotices(ctx)
	if err != nil || len(notices) != 1 {
		t.Fatalf("commit-before-notify recovery %+v %v", notices, err)
	}
}

func TestReadOnlyCommandEvidenceIsNotMissingOrPassingVerification(t *testing.T) {
	daemon, store, owner, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": kernel.RunEvidence{ToolName: "terminal", Kind: "command", Status: "succeeded", Process: &kernel.ToolProcessResult{Started: true, ExitCode: new(int)}, StartedAt: 10, FinishedAt: 20, Command: &kernel.CommandEvidence{Command: "custom-check", Kind: "command"}}})})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := daemon.coordinator().evidenceOutcome(ctx, owner.TenantID, run.ID)
	if got == nil || got.State != "not_applicable" || got.OrdinaryCommands != 1 || len(got.Checks) != 0 || !strings.Contains(got.Summary, "were observed") {
		t.Fatalf("ordinary read evidence %+v", got)
	}
	mismatches := verificationClaimMismatches(api.RunOutcome{Tests: []string{"smoke check passed"}, Verification: got})
	if len(mismatches) != 1 || !strings.Contains(mismatches[0], "were executed") {
		t.Fatalf("execution mistaken for missing/passing evidence: %v", mismatches)
	}
}
