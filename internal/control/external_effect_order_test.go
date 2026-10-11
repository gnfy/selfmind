package control

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
)

func orderedWatch(t *testing.T, store *Store, owner *IdentityContext, run *Run, command string) *ExternalWatch {
	t.Helper()
	watch, err := store.CreateExternalWatch(context.Background(), ExternalWatch{
		TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: run.TaskID, RunID: run.ID,
		Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^SUCCEEDED$",
		PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion,
			CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return watch
}

// The second-resolution timestamps cannot tell a watcher registered just
// before a claim from one registered just after it. The sequence can.
func TestClaimAndWatcherSequenceOrdersSameSecondInserts(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, _, _ := externalEffectRuns(t, store)
	before := orderedWatch(t, store, owner, runA, "printf SUCCEEDED")
	claims := uncertainEffect(t, store, owner, runA, "effect-deploy", "deploy:production")
	after := orderedWatch(t, store, owner, runA, "printf SUCCEEDED; true")
	if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET created_at = 1000 WHERE id IN (?, ?)`, before.ID, after.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE external_effect_claims SET created_at = 1000 WHERE id = ?`, claims[0].ID); err != nil {
		t.Fatal(err)
	}
	claim := claims[0]
	if before.Sequence <= 0 || claim.Sequence <= before.Sequence || after.Sequence <= claim.Sequence {
		t.Fatalf("insert order watcher=%d claim=%d watcher=%d is not strictly increasing", before.Sequence, claim.Sequence, after.Sequence)
	}
	// Every reader of the stored types reports the same sequence.
	for _, watch := range []*ExternalWatch{before, after} {
		stored, err := store.GetExternalWatch(ctx, owner.TenantID, watch.ID)
		if err != nil || stored == nil || stored.Sequence != watch.Sequence || stored.CreatedAt.Unix() != 1000 {
			t.Fatalf("stored watcher %s = %+v err=%v", watch.ID, stored, err)
		}
	}
	unresolved, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 10)
	if err != nil || len(unresolved) != 1 || unresolved[0].Sequence != claim.Sequence {
		t.Fatalf("unresolved claims = %+v err=%v", unresolved, err)
	}
	observing, err := store.ExternalEffectForObservation(ctx, owner.TenantID, owner.PersonID, runA.ID, claim.ID)
	if err != nil || observing.Sequence != claim.Sequence {
		t.Fatalf("observation lookup = %+v err=%v", observing, err)
	}
	replay, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: runA.ID, EffectID: "effect-deploy", TargetKeys: []string{"deploy:production"}})
	if err != nil || !replay.AlreadyKnown || replay.Claims[0].Sequence != claim.Sequence {
		t.Fatalf("known effect replay = %+v err=%v", replay, err)
	}
}

// One counter orders claims, watchers, and events across connections. Every
// target of one atomic claim shares its sequence, and the blocking claims
// reported to a refused Run carry theirs.
func TestEffectSequenceIsOneCounterAcrossConnectionsAndEvents(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	first, err := OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	owner, runA, runB, runC := externalEffectRuns(t, first)
	group := uncertainEffect(t, first, owner, runA, "effect-sync", "cluster:west", "cluster:east")
	if len(group) != 2 || group[0].Sequence != group[1].Sequence || group[0].Sequence <= 0 {
		t.Fatalf("one atomic claim has sequences %d and %d", group[0].Sequence, group[1].Sequence)
	}
	event, err := second.AppendEvent(ctx, Event{TaskID: runB.TaskID, RunID: runB.ID, Type: "note", Visibility: "task"})
	if err != nil {
		t.Fatal(err)
	}
	watch := orderedWatch(t, second, owner, runB, "printf SUCCEEDED")
	if event.Cursor <= group[0].Sequence || watch.Sequence <= event.Cursor {
		t.Fatalf("claim=%d event=%d watcher=%d do not share one increasing counter", group[0].Sequence, event.Cursor, watch.Sequence)
	}
	refused, err := second.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: runC.ID, EffectID: "effect-east", TargetKeys: []string{"cluster:east"}})
	if err != nil || refused.Granted || len(refused.BlockingClaims) == 0 || refused.BlockingClaims[0].Sequence != group[0].Sequence {
		t.Fatalf("refused claim = %+v err=%v", refused, err)
	}
	later := uncertainEffect(t, first, owner, runB, "effect-later", "registry:npm")
	if later[0].Sequence <= watch.Sequence {
		t.Fatalf("later claim %d is not after watcher %d", later[0].Sequence, watch.Sequence)
	}
}
