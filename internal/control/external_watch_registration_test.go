package control

import (
	"context"
	"testing"
	"time"
)

func TestWatchGroupKeepsAlreadyObservedMembersAcrossReload(t *testing.T) {
	for _, secondState := range []string{ExternalWatchPending, ExternalWatchSucceeded, ExternalWatchFailed, "missing"} {
		t.Run(secondState, func(t *testing.T) {
			ctx := context.Background()
			store, identity, task, run := newRecoveryFixture(t)
			group, err := store.ResolveOrCreateExternalWatchGroup(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID, "targets", ExternalWatchGroupAll, 2)
			if err != nil {
				t.Fatal(err)
			}
			makeWatch := func(command, status string) *ExternalWatch {
				w, e := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID, CWD: t.TempDir(), Command: command, SuccessPattern: "DONE", WaitGroupID: group.ID, Status: status, LastOutput: "DONE", TimeoutAt: time.Now().Add(time.Hour), PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion}})
				if e != nil {
					t.Fatal(e)
				}
				return w
			}
			first := makeWatch("one", ExternalWatchSucceeded)
			reloaded, err := store.GetExternalWatch(ctx, identity.TenantID, first.ID)
			if err != nil || reloaded.Status != ExternalWatchSucceeded || reloaded.LastOutput != "DONE" || reloaded.FinishedAt == nil {
				t.Fatalf("lost initial evidence: %+v %v", reloaded, err)
			}
			missing, err := store.IncompleteRunWatchGroups(ctx, identity.TenantID, run.ID)
			if err != nil || len(missing) != 1 {
				t.Fatalf("missing=%v err=%v", missing, err)
			}
			var second *ExternalWatch
			if secondState != "missing" {
				second = makeWatch("two", secondState)
			}
			if r, e := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, first.ID); e != nil || r.Terminal {
				t.Fatalf("settled before handoff: %+v %v", r, e)
			}
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_external"); err != nil {
				t.Fatal(err)
			}
			if secondState == ExternalWatchPending {
				if r, e := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, first.ID); e != nil || r.Terminal {
					t.Fatalf("settled before second: %+v %v", r, e)
				}
				if _, e := store.FinishExternalWatch(ctx, identity.TenantID, second.ID, ExternalWatchSucceeded, "DONE", ""); e != nil {
					t.Fatal(e)
				}
			}
			want := ExternalWatchSucceeded
			if secondState == ExternalWatchFailed {
				want = ExternalWatchFailed
			}
			if secondState == "missing" {
				want = ExternalWatchBlocked
			}
			r, e := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, first.ID)
			if e != nil || !r.Terminal || !r.Won || r.Status != want {
				t.Fatalf("resolution=%+v %v", r, e)
			}
			again, e := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, first.ID)
			if e != nil || again.Won || !again.Terminal || again.Status != want {
				t.Fatalf("duplicate resolution=%+v %v", again, e)
			}
		})
	}
}

func TestWaitBacklogIncludesTerminalMemberOfIncompleteGroup(t *testing.T) {
	ctx := context.Background()
	s, i, task, run := newRecoveryFixture(t)
	g, e := s.ResolveOrCreateExternalWatchGroup(ctx, i.TenantID, i.PersonID, task.ID, run.ID, "pair", ExternalWatchGroupAll, 2)
	if e != nil {
		t.Fatal(e)
	}
	w, e := s.CreateExternalWatch(ctx, ExternalWatch{TenantID: i.TenantID, PersonID: i.PersonID, TaskID: task.ID, RunID: run.ID, CWD: t.TempDir(), Command: "check one", SuccessPattern: "DONE", WaitGroupID: g.ID, Status: ExternalWatchSucceeded, LastOutput: "DONE", PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion}})
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.ExternalWaitBacklogForPerson(ctx, i.TenantID, i.PersonID)
	if e != nil || b.PendingGroups != 1 || b.IncompleteGroups != 1 || b.UnfinalizedMembers != 1 || b.OldestGroupAt.IsZero() {
		t.Fatalf("missing backlog: %+v %v", b, e)
	}
	b, e = s.ExternalWaitBacklogForPerson(ctx, i.TenantID, "another-person")
	if e != nil || b.PendingGroups != 0 || b.UnfinalizedMembers != 0 {
		t.Fatalf("cross-person backlog: %+v %v", b, e)
	}
	w.ID = ""
	if _, e = s.CreateExternalWatch(ctx, *w); e == nil {
		t.Fatal("duplicate observation filled the missing target")
	}
}
