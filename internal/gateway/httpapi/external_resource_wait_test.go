package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestExternalResourceWaitWakeupQueuesExactContinuation(t *testing.T) {
	daemon, store, identity, taskA, _ := newApprovalTestServer(t)
	ctx := context.Background()
	runA, err := store.StartRunWithOptions(ctx, taskA, "cli", "first operation", control.StartRunOptions{MaxActiveRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "second", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	runB, err := store.StartRunWithOptions(ctx, taskB, "cli", "second operation", control.StartRunOptions{MaxActiveRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(run *control.Run, effect string) control.ExternalEffectClaimDecision {
		t.Helper()
		decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: identity.TenantID,
			PersonID: identity.PersonID, RunID: run.ID, EffectID: effect, TargetKeys: []string{"remote:shared"}})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	if !claim(runA, "first").Granted || claim(runB, "second").Granted {
		t.Fatal("setup did not establish a blocked target")
	}
	if err := store.FinishRun(ctx, identity.TenantID, runB.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	daemon.runExternalResourceWaitPass(ctx)
	key := "external-resource:" + runB.ID + ":second"
	if row, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, key); err != nil || row != nil {
		t.Fatalf("occupied target scheduled a wakeup: %+v %v", row, err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, identity.TenantID, identity.PersonID, runA.ID, "first", "observer:terminal"); err != nil {
		t.Fatal(err)
	}
	// Occupy the in-memory foreground slot so the wakeup remains inspectable
	// in the durable queue; the worker itself must not run the agent inline.
	active := &activeRun{PersonID: identity.PersonID, RunID: runA.ID, TaskID: taskA.ID, Channel: "cli"}
	if !daemon.coordinator().beginActive(identity.PersonID, active) {
		t.Fatal("could not hold foreground slot")
	}
	defer daemon.coordinator().endActiveRun(identity.PersonID, active)
	daemon.runExternalResourceWaitPass(ctx)
	daemon.runExternalResourceWaitPass(ctx)
	row, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, key)
	if err != nil || row == nil || row.TaskID != taskB.ID || row.ReplyToRunID != runB.ID || row.Status != control.QueueStatusQueued {
		t.Fatalf("wakeup did not preserve exact owner/parent: %+v %v", row, err)
	}
}

func TestExternalResourceWakeupStartsExactDaemonChild(t *testing.T) {
	provider := newSlowLLMProvider("I checked the target and completed the remaining work.")
	daemon, store, _ := newDetachedRunServer(t, provider)
	provider.releaseNow()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	makeRun := func(title string) (*control.Task, *control.Run) {
		t.Helper()
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID,
			PersonID: identity.PersonID, Title: title, Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRunWithOptions(ctx, task, "cli", title, control.StartRunOptions{MaxActiveRuns: 2})
		if err != nil {
			t.Fatal(err)
		}
		return task, run
	}
	_, first := makeRun("first")
	task, waiting := makeRun("second")
	for _, entry := range []struct {
		run         *control.Run
		effect      string
		wantGranted bool
	}{
		{first, "effect-first", true}, {waiting, "effect-wait", false},
	} {
		decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			RunID: entry.run.ID, EffectID: entry.effect, TargetKeys: []string{"remote:shared"},
		})
		if err != nil || decision.Granted != entry.wantGranted {
			t.Fatalf("claim: %+v %v", decision, err)
		}
	}
	if err := store.FinishRun(ctx, identity.TenantID, first.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, waiting.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, identity.TenantID, identity.PersonID, first.ID, "effect-first", "observer:terminal"); err != nil {
		t.Fatal(err)
	}
	daemon.runExternalResourceWaitPass(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
		if err != nil {
			return false
		}
		childFinished := false
		for _, run := range runs {
			if run.ResumesRunID == waiting.ID && run.Status != "running" {
				childFinished = true
			}
		}
		if !childFinished {
			return false
		}
		events, err := store.ListTaskEvents(ctx, task.ID, 100)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Type != "run.started" || event.RunID == waiting.ID {
				continue
			}
			var payload struct {
				Origin string `json:"origin"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Origin == runOriginResource {
				return true
			}
		}
		return false
	}, "resource wakeup did not finish an exact daemon-origin child")
}
