package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestExternalWatchFinalizationClaimBeforeRunDoesNotBlock(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"})
	defer daemon.coordinator().endActive(identity.PersonID)
	watch, run := seedConcludedWatch(t, store, identity, task)
	if err := daemon.enqueueExternalWatchFinalization(ctx, *watch, identity, "observed success"); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetQueuedByIdempotencyKey(ctx, watch.TenantID, externalWatchFinalizationKey(*watch))
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimQueued(ctx, identity.TenantID, row.ID, time.Minute); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	daemon.reconcileExternalWatchFinalizations(ctx)
	assertWatcherRunState(t, store, identity, run.ID, "waiting_external", "")
	if hasEventOfType(t, store, task.ID, "external_watch.finalization_blocked") {
		t.Fatal("a valid claim without a materialized Run was reported as failure")
	}
	row, err = store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || row.Status != control.QueueStatusStarted || row.Restarts != 0 {
		t.Fatalf("live claim changed: %+v %v", row, err)
	}
}

func TestExternalWatchContextIncludesExactParentTargets(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "observe two operations")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"operation-a", "operation-b"} {
		w, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: parent.ID,
			CWD: t.TempDir(), Command: "true", SuccessPattern: "SUCCESS",
			PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: 1, Target: target},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FinishExternalWatch(ctx, identity.TenantID, w.ID, control.ExternalWatchSucceeded, "SUCCESS", ""); err != nil {
			t.Fatal(err)
		}
	}
	unrelated, err := store.StartRun(ctx, task, "cli", "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: unrelated.ID, CWD: t.TempDir(), Command: "true", SuccessPattern: "SUCCESS", PreflightReceipt: control.ExternalWatchPreflightReceipt{Target: "unrelated-target"}}); err != nil {
		t.Fatal(err)
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, nil, nil, "cli", "cli", "finish", attachContextFull, parent)
	if len(selected.ExternalWatches) != 2 {
		t.Fatalf("exact parent evidence: %+v", selected.ExternalWatches)
	}
	prompt := selected.Prompt(8000)
	for _, target := range []string{"operation-a", "operation-b", "succeeded"} {
		if !strings.Contains(prompt, target) {
			t.Fatalf("missing %s: %s", target, prompt)
		}
	}
	if strings.Contains(prompt, "unrelated-target") {
		t.Fatal("cross-run watch evidence leaked")
	}
	selected = daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, nil, nil, "cli", "cli", "new work", attachContextNone, nil)
	if len(selected.ExternalWatches) != 0 {
		t.Fatal("new work inherited external evidence")
	}
}
