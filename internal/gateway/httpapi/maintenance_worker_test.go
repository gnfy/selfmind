package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
)

func TestMaintenanceWorkerDoesNotSkipFreshJobBeforePayloadAttach(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	task := seedTask(t, store, identity, "fresh terminal task", "done", 1)
	runs, err := store.ListTaskRuns(context.Background(), identity.TenantID, task.ID, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	daemon.PostRunAnalyzer = &fakeLabeler{reply: "KEEP"}
	daemon.runMaintenancePass(context.Background())
	job, err := store.GetMaintenanceJob(context.Background(), identity.TenantID, runs[0].ID, postRunAnalyzerVersion)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if job.Status != control.MaintenanceJobPending {
		t.Fatalf("fresh payload race must remain pending, got %+v", job)
	}
}
