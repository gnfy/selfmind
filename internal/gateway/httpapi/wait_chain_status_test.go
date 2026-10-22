package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

func TestWaitingParentStatusFollowsExactLeaf(t *testing.T) {
	for _, leafStatus := range []string{"running", "waiting_user", "waiting_external", "blocked", "verification_partial", "done"} {
		t.Run(leafStatus, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Exact waiting chain", Channel: "session-a"})
			if err != nil {
				t.Fatal(err)
			}
			parent := seedUnresolvedRun(t, store, identity.TenantID, task, "session-a", "original request", "waiting_user")
			middle, err := store.StartRunWithOptions(ctx, task, "session-a", "approval continuation", control.StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, identity.TenantID, middle.ID, "blocked"); err != nil {
				t.Fatal(err)
			}
			leaf, err := store.StartRunWithOptions(ctx, task, "session-a", "resource continuation", control.StartRunOptions{ResumesRunID: middle.ID})
			if err != nil {
				t.Fatal(err)
			}
			if leafStatus != "running" {
				if err := store.FinishRun(ctx, identity.TenantID, leaf.ID, leafStatus); err != nil {
					t.Fatal(err)
				}
			}
			// Newer work in the same Thread is not part of the waiting chain.
			unrelated := seedUnresolvedRun(t, store, identity.TenantID, task, "session-b", "unrelated sibling", "interrupted")
			d := &Server{Control: store, DefaultTenantID: "default"}
			card, err := d.statusRunReply(ctx, identity, parent.ID)
			if err != nil || !strings.Contains(card, shortRunID(leaf.ID)) || !strings.Contains(card, leafStatus) || strings.Contains(card, shortRunID(unrelated.ID)) {
				t.Fatalf("parent did not display exact leaf: %q err=%v", card, err)
			}
			if leafStatus != "done" && strings.Contains(card, "· done") {
				t.Fatalf("unfinished leaf reported complete: %q", card)
			}
			for id, want := range map[string]string{parent.ID: "waiting_user", middle.ID: "blocked", leaf.ID: leafStatus} {
				run, err := store.GetRun(ctx, identity.TenantID, id)
				if err != nil || run.Status != want {
					t.Fatalf("display changed history: run=%+v err=%v", run, err)
				}
			}
			if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey, task.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumeRunPinKey, parent.ID); err != nil {
				t.Fatal(err)
			}
			pinned, err := d.statusReply(ctx, identity)
			if err != nil || !strings.Contains(pinned, shortRunID(leaf.ID)) || !strings.Contains(pinned, leafStatus) {
				t.Fatalf("pinned parent ignored leaf: %q err=%v", pinned, err)
			}
			pin, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumeRunPinKey)
			if pin != parent.ID {
				t.Fatalf("display changed resume authority: %q", pin)
			}
		})
	}
}

func TestWaitingParentWithoutChildKeepsWaitStatus(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "No continuation yet", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "await an answer", "waiting_user")
	seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "independent completed work", "done")
	d := &Server{Control: store, DefaultTenantID: "default"}
	card, err := d.statusRunReply(ctx, identity, parent.ID)
	if err != nil || !strings.Contains(card, "waiting_user") || !strings.Contains(card, shortRunID(parent.ID)) || strings.Contains(card, "· done") {
		t.Fatalf("childless wait invented completion: %q err=%v", card, err)
	}
}
