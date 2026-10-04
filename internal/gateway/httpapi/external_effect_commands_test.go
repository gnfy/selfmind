package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestEffectsControlIsPersonScopedAndReleasesOnlyFinalizedWatcher(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	owner := &control.IdentityContext{TenantID: "default", PersonID: "effect-owner", Platform: "cli"}
	other := &control.IdentityContext{TenantID: "default", PersonID: "effect-stranger", Platform: "cli"}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "deploy", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "deploy", control.StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{
		TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID,
		EffectID: "effect-deploy", TargetKeys: []string{control.UnknownExternalTarget},
	})
	if err != nil || !claim.Granted {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	command := "printf SUCCEEDED"
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^SUCCEEDED$",
		PreflightReceipt: control.ExternalWatchPreflightReceipt{
			Version:     control.ExternalWatchContinuationReceiptVersion,
			CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command))),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: "default"}
	commandReply := func(identity *control.IdentityContext, content string) string {
		t.Helper()
		handled, reply, _, err := server.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: "cli", Content: content})
		if err != nil || !handled {
			t.Fatalf("%s: handled=%v err=%v", content, handled, err)
		}
		return reply
	}
	if reply := commandReply(other, "/effects"); !strings.Contains(reply, "No unresolved") || strings.Contains(reply, claim.Claims[0].ID) {
		t.Fatalf("stranger saw effect: %q", reply)
	}
	if reply := commandReply(owner, "/effects"); !strings.Contains(reply, claim.Claims[0].ID) {
		t.Fatalf("owner could not find effect: %q", reply)
	}
	resolve := "/effects resolve " + claim.Claims[0].ID + " " + watch.ID
	if reply := commandReply(owner, resolve); !strings.Contains(reply, "remains occupied") {
		t.Fatalf("unfinalized watcher released effect: %q", reply)
	}
	if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCEEDED", ""); err != nil || !ok {
		t.Fatalf("finish watch: %v %v", ok, err)
	}
	if ok, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, watch.ID); err != nil || !ok {
		t.Fatalf("finalize watch: %v %v", ok, err)
	}
	if reply := commandReply(other, resolve); strings.Contains(reply, "Released") {
		t.Fatalf("stranger released effect: %q", reply)
	}
	if reply := commandReply(owner, resolve); !strings.Contains(reply, "Released external effect") {
		t.Fatalf("finalized watcher did not release effect: %q", reply)
	}
	if reply := commandReply(owner, "/effects"); !strings.Contains(reply, "No unresolved") {
		t.Fatalf("resolved effect remained listed: %q", reply)
	}
}
