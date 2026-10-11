package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestStopCancelsParkedHumanWaitThroughMessagePath(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		for _, exact := range []bool{true, false} {
			name := kind + "/exact"
			if !exact {
				name = kind + "/own-session"
			}
			t.Run(name, func(t *testing.T) {
				daemon, store, provider, owner := newHumanWaitServer(t)
				daemon.HumanWaitGrace = 20 * time.Millisecond
				provider.clarification = kind == "clarification"
				ws := humanWaitWorkspace(t, store, owner, "stop-wait")
				startHumanWait(t, daemon, ws, "origin")
				ctx := context.Background()
				var runID, ref string
				waitUntil(t, time.Second, func() bool {
					if kind == "approval" {
						asks, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
						if len(asks) == 1 && asks[0].WaiterState == "parked" {
							runID, ref = asks[0].RunID, asks[0].ID
						}
					} else {
						asks, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
						if len(asks) == 1 && asks[0].ParkedAt != nil {
							runID, ref = asks[0].RunID, asks[0].ID
						}
					}
					return runID != "" && daemon.coordinator().activeCount(owner.PersonID) == 0
				}, "human wait did not park")
				content := "/stop"
				if exact {
					content += " " + runID
				}
				response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin", Content: content})
				run, err := store.GetRun(ctx, owner.TenantID, runID)
				requested, _ := store.RunCancelRequested(ctx, owner.TenantID, runID)
				if code != 200 || err != nil || run == nil || run.Status != "cancelled" || !requested {
					t.Fatalf("parked human wait survived /stop: response=%+v code=%d run=%+v err=%v", response, code, run, err)
				}
				if kind == "approval" {
					ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
					if ask.Status != "expired" {
						t.Fatalf("cancelled Run kept a pending approval: %+v", ask)
					}
					if _, err := daemon.respondApprovalByToken(ctx, owner, ref, "approved", "origin", control.ApprovalDecisionInput{}); err == nil {
						t.Fatal("cancelled approval accepted a late decision")
					}
				} else {
					ask, _ := store.GetClarifyRequest(ctx, owner.TenantID, ref)
					if ask.Status != "expired" {
						t.Fatalf("cancelled Run kept a pending question: %+v", ask)
					}
					if _, err := daemon.answerClarifyTarget(ctx, owner, ask, "json", "origin"); err == nil {
						t.Fatal("cancelled question accepted a late answer")
					}
				}
				roots, err := store.ListRunRootReservations(ctx, owner.TenantID, owner.PersonID)
				if err != nil || len(roots) != 0 || daemon.coordinator().activeCount(owner.PersonID) != 0 {
					t.Fatalf("cancelled human wait retained execution: roots=%+v err=%v", roots, err)
				}
				if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
					t.Fatalf("unapproved effect occurred: %v", err)
				}
			})
		}
	}
}

func TestParkedHumanStopRequiresSourceOrExactOwnedRun(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	daemon.HumanWaitGrace = 20 * time.Millisecond
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "stop-owner")
	startHumanWait(t, daemon, ws, "origin")
	approval := waitParkedApproval(t, daemon, store, owner)
	if err := store.SetPersonSetting(ctx, owner.TenantID, owner.PersonID, resumeRunPinKey, approval.RunID); err != nil {
		t.Fatal(err)
	}
	for _, req := range []api.MessageRequest{
		{Platform: "cli", PlatformUserID: "local", Channel: "other-window", Content: "/stop"},
		{Platform: "cli", PlatformUserID: "another-person", Channel: "origin", Content: "/stop " + approval.RunID},
	} {
		daemon.ProcessMessage(ctx, req)
		run, _ := store.GetRun(ctx, owner.TenantID, approval.RunID)
		ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
		if run.Status != "waiting_user" || ask.Status != "pending" {
			t.Fatalf("unselected/foreign work was cancelled by %+v: run=%+v request=%+v", req, run, ask)
		}
	}
	if err := store.SetPersonSetting(ctx, owner.TenantID, owner.PersonID, resumeRunPinKey, ""); err != nil {
		t.Fatal(err)
	}
	other := humanWaitWorkspace(t, store, owner, "second-wait")
	startHumanWait(t, daemon, other, "other-window")
	waitUntil(t, time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		return len(pending) == 2 && pending[0].WaiterState == "parked" && pending[1].WaiterState == "parked" && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "second window did not park")
	response, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin", Content: "/stop"})
	run, _ := store.GetRun(ctx, owner.TenantID, approval.RunID)
	if run.Status != "cancelled" {
		t.Fatalf("other window made own-session stop ambiguous: %+v %q", run, response.Content)
	}
	remaining, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
	if len(remaining) != 1 || remaining[0].RequestedChannel != "other-window" {
		t.Fatalf("own-session stop touched another window: %+v", remaining)
	}
	response, _ = daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin", Content: "/stop " + shortRunID(remaining[0].RunID)})
	run, _ = store.GetRun(ctx, owner.TenantID, remaining[0].RunID)
	if run.Status != "cancelled" {
		t.Fatalf("explicit owned short Run could not be stopped from another window: %+v %q", run, response.Content)
	}
}

func TestOwnParkedHumanStopDoesNotCancelAnotherActiveWindow(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	daemon.HumanWaitGrace = 20 * time.Millisecond
	ctx := context.Background()
	first := humanWaitWorkspace(t, store, owner, "parked-origin")
	startHumanWait(t, daemon, first, "origin")
	parked := waitParkedApproval(t, daemon, store, owner)
	// Keep the second window's original stack alive. The first window must
	// still address its own durable wait, not this remaining cancel handle.
	daemon.HumanWaitGrace = time.Hour
	second := humanWaitWorkspace(t, store, owner, "active-other")
	startHumanWait(t, daemon, second, "other-window")
	var activeID string
	waitUntil(t, time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		for _, ask := range pending {
			if ask.RequestedChannel == "other-window" && ask.WaiterState == "live" {
				activeID = ask.RunID
			}
		}
		return activeID != "" && daemon.coordinator().activeCount(owner.PersonID) == 1
	}, "other window was not active")
	response, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin", Content: "/stop"})
	firstRun, _ := store.GetRun(ctx, owner.TenantID, parked.RunID)
	secondRun, _ := store.GetRun(ctx, owner.TenantID, activeID)
	requested, _ := store.RunCancelRequested(ctx, owner.TenantID, activeID)
	if firstRun.Status != "cancelled" || secondRun.Status != "running" || requested || daemon.coordinator().activeCount(owner.PersonID) != 1 {
		t.Fatalf("own wait stop chose another window: parked=%+v active=%+v requested=%v response=%q", firstRun, secondRun, requested, response.Content)
	}
}
