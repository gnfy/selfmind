package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestModernWatchGroupAndApprovalEvidenceSurviveStoreRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.ResolveOrCreateAccount(ctx, "default", "cli", "restart", "Restart")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, TaskCreate{TenantID: i.TenantID, PersonID: i.PersonID, Title: "await prerequisites", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartRun(ctx, task, "cli", "prepare receipt")
	if err != nil {
		t.Fatal(err)
	}
	intent := json.RawMessage(`{"version":1,"snapshot":{"source":"direct","explicit_deny":["do not modify config.yaml"]}}`)
	payload, _ := json.Marshal(map[string]interface{}{"approval_intent": intent})
	if _, err := s.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.started", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	group, err := s.ResolveOrCreateExternalWatchGroup(ctx, i.TenantID, i.PersonID, task.ID, run.ID, "pair", ExternalWatchGroupAll, 2)
	if err != nil {
		t.Fatal(err)
	}
	var watches []*ExternalWatch
	for _, state := range []string{ExternalWatchSucceeded, ExternalWatchPending} {
		w, err := s.CreateExternalWatch(ctx, ExternalWatch{TenantID: i.TenantID, PersonID: i.PersonID, TaskID: task.ID, RunID: run.ID, CWD: dir, Command: "printf " + state, SuccessPattern: "DONE", WaitGroupID: group.ID, Status: state, LastOutput: "DONE", TimeoutAt: time.Now().Add(time.Hour), PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion}})
		if err != nil {
			t.Fatal(err)
		}
		watches = append(watches, w)
	}
	if err := s.FinishRun(ctx, i.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.RunApprovalIntent(ctx, i.TenantID, i.PersonID, run.ID)
	if err != nil || string(got) != string(intent) {
		t.Fatalf("intent lost: %s %v", got, err)
	}
	if other, err := s.RunApprovalIntent(ctx, i.TenantID, "stranger", run.ID); err != nil || len(other) != 0 {
		t.Fatalf("cross-person intent: %s %v", other, err)
	}
	first, err := s.GetExternalWatch(ctx, i.TenantID, watches[0].ID)
	if err != nil || first.Status != ExternalWatchSucceeded || first.PreflightReceipt.Version != ExternalWatchContinuationReceiptVersion {
		t.Fatalf("lost preflight: %+v %v", first, err)
	}
	if verdict, err := s.ResolveExternalWatchGroup(ctx, i.TenantID, group.ID, first.ID); err != nil || verdict.Terminal {
		t.Fatalf("early aggregate: %+v %v", verdict, err)
	}
	if _, err := s.FinishExternalWatch(ctx, i.TenantID, watches[1].ID, ExternalWatchSucceeded, "DONE", ""); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		verdict, err := s.ResolveExternalWatchGroup(ctx, i.TenantID, group.ID, first.ID)
		if err != nil || !verdict.Terminal || verdict.Status != ExternalWatchSucceeded || verdict.Won != (attempt == 0) {
			t.Fatalf("attempt %d: %+v %v", attempt, verdict, err)
		}
	}
}
