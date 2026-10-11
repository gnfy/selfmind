package httpapi

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestParkedHumanWaitRechecksAfterOverlappingWork(t *testing.T) {
	daemon, store, provider, owner := newHumanWaitServer(t)
	daemon.HumanWaitGrace = 25 * time.Millisecond
	provider.readFirst = true
	ctx := context.Background()
	first := humanWaitWorkspace(t, store, owner, "first-root")
	if err := os.WriteFile(filepath.Join(first.LocalPath, "input.txt"), []byte("before parking"), 0600); err != nil {
		t.Fatal(err)
	}
	startHumanWait(t, daemon, first, "origin")
	approval := waitParkedApproval(t, daemon, store, owner)
	waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "worker not released")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "other",
		WorkspaceID: first.ID, Content: "independent work", ForceNew: true, Async: true})
	if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.QueueID != "" {
		t.Fatalf("same-root work queued: %d %+v", code, response)
	}
	waitUntil(t, time.Second, func() bool {
		return provider.independent.Load() == 1 && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "intervening Run not complete")
	if err := os.WriteFile(filepath.Join(first.LocalPath, "input.txt"), []byte("after parking"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "origin", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		runs, _ := store.ListTaskRuns(ctx, owner.TenantID, approval.TaskID, 10)
		for _, run := range runs {
			if run.ResumesRunID == approval.RunID && run.Status == "done" {
				return daemon.coordinator().activeCount(owner.PersonID) == 0
			}
		}
		return false
	}, "answer did not finish exact continuation")
	if _, err := os.Stat(filepath.Join(first.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
		t.Fatalf("cached write dispatched after workspace activity: %v", err)
	}
	if !provider.retainedRead.Load() {
		t.Fatal("prior completed read was lost or replayed")
	}
	events, err := store.ListTaskEvents(ctx, approval.TaskID, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "tool.completed" && strings.Contains(string(event.Payload), "resume_revalidation_required") {
			found = true
		}
	}
	if !found {
		t.Fatal("Main did not receive typed revalidation evidence")
	}
}

func TestHumanCheckpointFaultRetainsRootUntilActualCancellation(t *testing.T) {
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_human_checkpoint BEFORE INSERT ON loop_checkpoints WHEN NEW.outcome='await_human' BEGIN SELECT RAISE(ABORT,'injected checkpoint write failure'); END`); err != nil {
		t.Fatal(err)
	}
	daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
	daemon.HumanWaitGrace = 25 * time.Millisecond
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "fault-reserved")
	other := humanWaitWorkspace(t, store, owner, "fault-independent")
	startHumanWait(t, daemon, ws, "fault-origin")
	var approval control.ApprovalRequest
	waitUntil(t, time.Second, func() bool {
		asks, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		if len(asks) != 1 {
			return false
		}
		approval = asks[0]
		events, _ := store.ListTaskEvents(ctx, approval.TaskID, 100)
		for _, event := range events {
			if event.Type == "checkpoint.failed" {
				return true
			}
		}
		return false
	}, "checkpoint fault was not observed")
	reservations, err := store.ListRunRootReservations(ctx, owner.TenantID, owner.PersonID)
	if err != nil || len(reservations) != 1 || reservations[0].RunID != approval.RunID || daemon.coordinator().activeCount(owner.PersonID) != 1 {
		t.Fatalf("failed checkpoint did not retain its executing worker/root: %+v %v", reservations, err)
	}
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "fault-overlap", WorkspaceID: ws.ID, Content: "independent work", ForceNew: true, Async: true})
	if code != 200 || response.Turn == nil || response.Turn.QueueID == "" {
		t.Fatalf("overlapping work bypassed the retained stack: %d %+v", code, response)
	}
	if !strings.Contains(response.Content, "overlapping writable directories") || !strings.Contains(response.Content, "/status "+approval.RunID) {
		t.Fatalf("queue acknowledgement hid the real blocker: %q", response.Content)
	}
	response, code = daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "fault-other", WorkspaceID: other.ID, Content: "independent work", ForceNew: true, Async: true})
	if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.QueueID != "" {
		t.Fatalf("retained stack consumed unrelated worker capacity: %d %+v", code, response)
	}
	waitUntil(t, time.Second, func() bool { return provider.independent.Load() == 1 }, "independent work did not use the other worker")
	response, code = daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "fault-origin", Content: "/stop " + approval.RunID})
	if code != 200 || response.Error != "" {
		t.Fatalf("exact cancellation failed: %d %+v", code, response)
	}
	waitUntil(t, 2*time.Second, func() bool {
		return provider.independent.Load() == 2 && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "actual cancellation did not release the retained root to queued work")
	reservations, err = store.ListRunRootReservations(ctx, owner.TenantID, owner.PersonID)
	if err != nil || len(reservations) != 0 {
		t.Fatalf("cancelled fault kept a root reservation: %+v %v", reservations, err)
	}
	if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
		t.Fatalf("cancelled unapproved effect ran: %v", err)
	}
}
