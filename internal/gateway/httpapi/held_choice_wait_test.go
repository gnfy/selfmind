package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestHeldChoiceDrainWaitsForExactProviderParent(t *testing.T) {
	ctx := context.Background()
	s := newAdmissionProbeServer(t)
	s.provider.skipApprovals = true
	s.setMode(t, "full-auto")
	im, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-local", "same person")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "Original deployment", Channel: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.store.StartRunWithOptions(ctx, task, "origin", "Original deployment", control.StartRunOptions{Admission: webAdmission("full-auto", "Original deployment")})
	if err != nil {
		t.Fatal(err)
	}
	original := api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local", Channel: "im-chat", Content: "also verify the deployed service", Async: true}
	snap, err := MultiRunChoiceRequest(original)
	if err != nil {
		t.Fatal(err)
	}
	choice, err := s.store.CreatePendingTurnChoice(ctx, control.PendingTurnChoiceCreate{TenantID: im.TenantID, PersonID: im.PersonID, AccountID: im.AccountID, Channel: original.Channel, RequestJSON: snap, Options: []control.TurnChoiceOption{{Key: "1", Label: "Original deployment", Action: "steer", TaskID: task.ID, RunID: parent.ID}, {Key: "2", Label: "new", Action: "new"}}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{TenantID: im.TenantID, PersonID: im.PersonID, TaskID: task.ID, RunID: parent.ID, Outcome: "continue_model", Detail: "provider_request", Snapshot: []byte(`[{"role":"user","content":"Original deployment"}]`)}); err != nil {
		t.Fatal(err)
	}
	providerWake := &control.QueuedTask{TenantID: im.TenantID, PersonID: im.PersonID, TaskID: task.ID, ReplyToRunID: parent.ID, Channel: "origin", Platform: "cli", PlatformUserID: "local", Content: "continue exact checkpoint", IdempotencyKey: "provider-wait:" + parent.ID, NotBefore: time.Now().Add(time.Hour), Admission: webAdmission("full-auto", "Original deployment")}
	if _, err := s.store.MaterializeRunFinalization(ctx, control.RunFinalization{Identity: *s.identity, RunID: parent.ID, TaskID: task.ID, RunStatus: "waiting_external", ExpectedRunStatus: "running", RequireCheckpoint: true, Continuation: providerWake, Event: control.Event{Payload: []byte(`{"outcome":{"status":"waiting_external","completion_reason":"provider_wait"},"provider_wait":{"reason":"rate_limit"}}`)}}); err != nil {
		t.Fatal(err)
	}
	response, refusal := s.daemon.routeMultiRunChoice(ctx, im, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local", Channel: "im-chat", Content: "1", Async: true}, choice, "1")
	if refusal != nil || response.Turn == nil || response.Turn.QueueID == "" {
		t.Fatalf("route: %+v %v", response, refusal)
	}
	row, err := s.store.GetQueued(ctx, im.TenantID, response.Turn.QueueID)
	if err != nil || row == nil || row.ReplyToRunID != parent.ID || row.Status != control.QueueStatusQueued || row.RunID != "" || s.daemon.coordinator().activeCount(im.PersonID) != 0 {
		t.Fatalf("supplement bypassed exact provider wait: row=%+v err=%v", row, err)
	}
	_, waiting, err := s.store.ResolveQueuedContinuation(ctx, *row)
	if err != nil || !waiting {
		t.Fatalf("supplement did not wait for original system child: wait=%v err=%v", waiting, err)
	}
}
