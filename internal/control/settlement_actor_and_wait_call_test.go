package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The audit names the endpoint a settle came from. A bounded channel is
// accepted when it is the actor's own account key or no account's key (a CLI
// session or a group chat), never when it is another account's key.
func TestSettlementChannelBelongsToItsActor(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, _, _ := externalEffectRuns(t, store)
	stranger, err := store.ResolveOrCreateAccount(ctx, owner.TenantID, "telegram", "424242", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	ownerIM, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "777", "Owner on Telegram")
	if err != nil {
		t.Fatal(err)
	}
	claims := uncertainEffect(t, store, owner, runA, "effect-publish", UnknownExternalTarget)
	settle := func(account, channel string) error {
		_, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
			ClaimID: claims[0].ID, ActorAccountID: account, ActorChannel: channel})
		return err
	}
	for name, channel := range map[string]string{
		"another account's key":    stranger.PlatformUserID,
		"own other account's key":  ownerIM.PlatformUserID,
		"unbounded channel":        strings.Repeat("c", 257),
		"channel of invalid bytes": "\xff\xfe",
		"channel of only blanks":   "   ",
	} {
		if err := settle(owner.AccountID, channel); !errors.Is(err, ErrExternalEffectSettlementActor) {
			t.Fatalf("%s: settle error = %v, want ErrExternalEffectSettlementActor", name, err)
		}
	}
	if count := settlementCount(t, store); count != 0 {
		t.Fatalf("refused settles wrote %d audit rows", count)
	}
	// The owner's IM account settling from its own private chat is attributable.
	if err := settle(ownerIM.AccountID, ownerIM.PlatformUserID); err != nil {
		t.Fatalf("own IM chat: %v", err)
	}
	var channel string
	if err := store.db.QueryRowContext(ctx, `SELECT actor_channel FROM external_effect_settlements`).Scan(&channel); err != nil || channel != "777" {
		t.Fatalf("audit channel = %q err=%v", channel, err)
	}
}

// A CLI session or group chat is no account's key.
func TestSettlementFromASessionOrGroupChannel(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	for run, channel := range map[*Run]string{runA: "6f1d1c9e-4f0b-4d55-9b3e-52b3b1f2a9c1", runB: "-1001234567890"} {
		claims := uncertainEffect(t, store, owner, run, "effect-"+run.ID, UnknownExternalTarget)
		if _, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
			ClaimID: claims[0].ID, ActorAccountID: owner.AccountID, ActorChannel: channel}); err != nil {
			t.Fatalf("settle from %s: %v", channel, err)
		}
	}
}

// A checkpoint's tool call is bounded, and where the approval records the
// call that asked, the binding must name that call.
func TestLoopCheckpointToolCallMatchesTheApproval(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "owner")
	payload, _ := json.Marshal(map[string]string{"tool": "terminal", "tool_call_id": "call_real"})
	recorded, err := store.CreateApprovalRequest(ctx, ApprovalRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: task.ID, RunID: run.ID, ActionType: "tool_call", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	unrecorded := pendingApproval(t, store, owner, task, run)
	for name, record := range map[string]LoopCheckpointRecord{
		"another call":          awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, recorded.ID, "call_other"),
		"unbounded call":        awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, unrecorded.ID, strings.Repeat("c", 257)),
		"call of invalid bytes": awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, unrecorded.ID, "\xff"),
	} {
		if err := store.SaveLoopCheckpoint(ctx, record); !errors.Is(err, ErrLoopCheckpointWaitBinding) {
			t.Fatalf("%s: save error = %v, want ErrLoopCheckpointWaitBinding", name, err)
		}
	}
	for _, record := range []LoopCheckpointRecord{
		awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, recorded.ID, "call_real"),
		awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, unrecorded.ID, strings.Repeat("c", 256)),
	} {
		if err := store.SaveLoopCheckpoint(ctx, record); err != nil {
			t.Fatalf("binding %s/%s: %v", record.WaitRef, record.WaitToolCallID[:8], err)
		}
	}
}
