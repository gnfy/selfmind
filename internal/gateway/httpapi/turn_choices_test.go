package httpapi

import (
	"context"
	"net/http"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func seedContinuityHistory(t *testing.T, store *control.Store, identity *control.IdentityContext, status string) (*control.Task, *control.Run) {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "RUQX-767 production release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "release PHP services for RUQX-767")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
		t.Fatal(err)
	}
	return task, run
}

func TestClaimedObserveChoiceIsReadOnly(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	_, run := seedContinuityHistory(t, store, identity, "completed")
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	choice, err := daemon.createTurnChoice(ctx, identity, api.MessageRequest{Channel: "cli", Content: "发布任务进展怎么样", ContinuityResolutionID: "turnres_source"}, []control.TurnChoiceOption{
		{Key: "1", Label: "release", Action: "observe", TaskID: run.TaskID, RunID: run.ID},
		{Key: "2", Label: "new", Action: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "1", ChoiceID: choice.ID,
	})
	if status != http.StatusOK || resp.Run == nil || resp.Run.ID != run.ID {
		t.Fatalf("status=%d response=%+v", status, resp)
	}
	runs, _ := store.ListTaskRuns(ctx, identity.TenantID, run.TaskID, 10)
	if len(runs) != 1 {
		t.Fatalf("observe choice started a run: %d", len(runs))
	}
	corrections, err := store.CountTurnResolutionCorrections(ctx, identity.TenantID, identity.PersonID, "turnres_source")
	if err != nil {
		t.Fatal(err)
	}
	if corrections != 1 {
		t.Fatalf("corrections=%d", corrections)
	}
}
