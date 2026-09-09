package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

func TestWatchFinalizationRestoresExecutionContract(t *testing.T) {
	provider := newSlowLLMProvider("Recorded the operation result.")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	task := parkEmptyTask(t, daemon, "generate a report")
	identity, err := store.ResolveOrCreateAccount(ctx, task.TenantID, "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	watch, parent := seedConcludedWatch(t, store, identity, task)
	if _, err := store.SyncRunPlan(ctx, task.TenantID, parent.ID, "deliver report", []control.RunPlanStepInput{
		{Step: "generate report", Status: "completed"},
		{Step: "record result", Status: "in_progress", SuccessCriteria: "report identifies the completed operation"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: parent.ID,
		Channel: "cli", Content: "Use the revised title: September draft", ContentHash: "revised-title",
	}); err != nil {
		t.Fatal(err)
	}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", TaskID: task.ID,
		ReplyToRunID: parent.ID, Origin: runOriginWatch, WatchID: watch.ID,
		ExecutionProfile: tools.ExecutionProfileWatchFinalization,
		Content:          externalWatchFinalizationContent(*watch, "operation succeeded"),
	})
	if status != 200 || resp.Run == nil {
		t.Fatalf("finalization: %d %+v", status, resp)
	}
	if resp.Run.ResumesRunID != parent.ID {
		t.Fatalf("lost exact parent: %+v", resp.Run)
	}
	plan, err := store.LatestRunPlan(ctx, task.TenantID, resp.Run.ID)
	if err != nil || plan == nil || len(plan.Steps) != 2 {
		t.Fatalf("lost plan: %+v %v", plan, err)
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, resp.Run, nil, "cli", "cli", "finish", attachContextFull, parent)
	prompt := selected.Prompt(8000)
	for _, want := range []string{"September draft", "record result", watch.ID} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("lost %q in context: %s", want, prompt)
		}
	}
}

func TestAnswerFileMentionsDoNotBecomeExecutionArtifacts(t *testing.T) {
	provider := newSlowLLMProvider("I updated /tmp/example.go and verified it.")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Explain how to update a file.",
	})
	if status != 200 || resp.Run == nil {
		t.Fatalf("answer: %d %+v", status, resp)
	}
	artifacts, err := store.ListRunArtifacts(context.Background(), resp.Identity.TenantID, resp.Identity.PersonID, resp.Task.ID, resp.Run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("prose promoted to artifacts: %+v", artifacts)
	}
}
