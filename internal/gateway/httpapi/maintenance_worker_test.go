package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/modelchange"
)

func TestMaintenancePassStopsWhenAReadyModelBecomesPending(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "ready boundary", "done", 1)
	runs, err := store.ListTaskRuns(context.Background(), identity.TenantID, task.ID, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if err := store.FinishRunWithMaintenancePayload(context.Background(), identity.TenantID,
		runs[0].ID, "done", postRunAnalyzerVersion, `{}`); err != nil {
		t.Fatal(err)
	}
	service, _ := testModelChangeService(t)
	if _, err := service.AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-pending"
	if _, err := service.Prepare(context.Background(), modelchange.PrepareRequest{
		Candidate: candidate, Source: "test", RequireConfirmation: true,
	}); err != nil {
		t.Fatal(err)
	}
	labeler := &fakeLabeler{reply: "KEEP"}
	daemon.PostRunAnalyzer = labeler
	daemon.ModelChanges = service
	daemon.runMaintenancePass(context.Background())
	labeler.mu.Lock()
	calls := labeler.calls
	labeler.mu.Unlock()
	if calls != 0 {
		t.Fatalf("maintenance analyzer calls = %d after Model Readiness became pending", calls)
	}
}

func TestMaintenanceWorkerSeesCurrentGenerationJob(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "fresh terminal task", "done", 1)
	runs, err := store.ListTaskRuns(context.Background(), identity.TenantID, task.ID, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if err := store.FinishRunWithMaintenancePayload(context.Background(), identity.TenantID,
		runs[0].ID, "done", postRunAnalyzerVersion, `{}`); err != nil {
		t.Fatal(err)
	}
	daemon.PostRunAnalyzer = &fakeLabeler{reply: "KEEP"}
	daemon.runMaintenancePass(context.Background())
	job, err := store.GetMaintenanceJob(context.Background(), identity.TenantID, runs[0].ID, postRunAnalyzerVersion)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if job.Status != control.MaintenanceJobPending {
		t.Fatalf("current-generation maintenance job must remain pending, got %+v", job)
	}
}
