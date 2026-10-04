package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProviderWaitFinalizationAtomicallyParksExactRunAndQueue(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "independent work", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "do the work",
		TaskID: task.ID, Class: QueueClassRecovery, IdempotencyKey: "recovery:source"})
	if err != nil {
		t.Fatal(err)
	}
	token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, source.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim source = %t %v", claimed, err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "session-a", "do the work", StartRunOptions{
		QueueID: source.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	wake := time.Now().Add(2 * time.Second)
	queue := &QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Platform: "cli", PlatformUserID: "local",
		Channel: "session-a", Content: "continue exact work", TaskID: task.ID, ReplyToRunID: run.ID,
		IdempotencyKey: "provider-wait:" + run.ID, NotBefore: wake}
	final := RunFinalization{Identity: *owner, RunID: run.ID, RunStatus: "waiting_external", TaskID: task.ID,
		TaskStatus: "in_progress", Channel: "session-a", Event: Event{Type: "run.finished"},
		Continuation: queue, ExpectedRunStatus: "running", RequireCheckpoint: true,
		ConsumedQueueID: source.ID, ConsumedQueueClaimToken: token}
	if _, err := store.MaterializeRunFinalization(ctx, final); err == nil {
		t.Fatal("wait parked without a durable pre-call checkpoint")
	}
	current, err := store.GetRun(ctx, owner.TenantID, run.ID)
	if err != nil || current.Status != "running" {
		t.Fatalf("partial park changed run: %+v %v", current, err)
	}
	if queued, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, queue.IdempotencyKey); err != nil || queued != nil {
		t.Fatalf("partial park enqueued work: %+v %v", queued, err)
	}
	if err := store.SaveLoopCheckpoint(ctx, LoopCheckpointRecord{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: task.ID, RunID: run.ID, ContractVersion: RunRecoveryContractVersion,
		Outcome: "continue_model", Detail: "provider_request", Snapshot: []byte(`[{"role":"user","content":"do the work"}]`)}); err != nil {
		t.Fatal(err)
	}
	stale := final
	stale.ConsumedQueueClaimToken = "stale"
	if _, err := store.MaterializeRunFinalization(ctx, stale); err == nil {
		t.Fatal("stale source queue claim parked the run")
	}
	if current, err := store.GetRun(ctx, owner.TenantID, run.ID); err != nil || current.Status != "running" {
		t.Fatalf("stale claim partially parked run: %+v %v", current, err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, final); err != nil {
		t.Fatal(err)
	}
	current, err = store.GetRun(ctx, owner.TenantID, run.ID)
	if err != nil || current.Status != "waiting_external" {
		t.Fatalf("parked run = %+v %v", current, err)
	}
	if consumed, err := store.GetQueued(ctx, owner.TenantID, source.ID); err != nil || consumed.Status != QueueStatusDone {
		t.Fatalf("source queue was not atomically settled: %+v %v", consumed, err)
	}
	queued, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, queue.IdempotencyKey)
	if err != nil || queued == nil || queued.ReplyToRunID != run.ID || queued.NotBefore.Before(wake) {
		t.Fatalf("exact delayed continuation = %+v %v", queued, err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "session-b", "steal parent", StartRunOptions{ResumesRunID: run.ID}); !errors.Is(err, ErrResumeTargetNotResumable) {
		t.Fatalf("unowned continuation claim = %v", err)
	}
	if resolved, waiting, err := store.ResolveQueuedContinuation(ctx, QueuedTask{TenantID: owner.TenantID,
		PersonID: owner.PersonID, ID: "another", TaskID: task.ID, ReplyToRunID: run.ID}); err != nil || !waiting || resolved.ReplyToRunID != run.ID {
		t.Fatalf("foreign queue passed provider wait: %+v waiting=%v err=%v", resolved, waiting, err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, final); err != nil {
		t.Fatalf("park replay should be idempotent: %v", err)
	}
	if count, err := store.CountQueued(ctx, owner.TenantID, owner.PersonID, QueueStatusQueued); err != nil || count != 1 {
		t.Fatalf("replay produced %d queue rows: %v", count, err)
	}
}
