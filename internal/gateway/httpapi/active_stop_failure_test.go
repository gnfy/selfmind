package httpapi

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestActiveStopPersistenceFailureLeavesLiveWaitCancellable(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "own session", true: "exact run"}[exact], func(t *testing.T) {
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			// This fixture writes DDL while the live worker saves its checkpoint.
			// Bound that setup wait instead of failing before fault injection.
			raw.SetMaxOpenConns(1)
			if _, err := raw.Exec(`PRAGMA busy_timeout=5000`); err != nil {
				t.Fatal(err)
			}
			daemon, _, _, owner := newHumanWaitServerWithStore(t, store)
			daemon.HumanWaitGrace = time.Hour
			ws := humanWaitWorkspace(t, store, owner, "live-stop-fault")
			startHumanWait(t, daemon, ws, "origin")
			ctx := context.Background()
			var runID, askID string
			waitUntil(t, time.Second, func() bool {
				asks, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
				if len(asks) == 1 && asks[0].WaiterState == "live" {
					runID, askID = asks[0].RunID, asks[0].ID
				}
				return runID != ""
			}, "live approval was not reached")
			if _, err := raw.Exec(`CREATE TRIGGER reject_active_stop BEFORE INSERT ON task_events WHEN NEW.type='approval.expired' BEGIN SELECT RAISE(ABORT,'injected stop failure'); END`); err != nil {
				t.Fatal(err)
			}
			content := "/stop"
			if exact {
				content += " " + runID
			}
			response, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin", Content: content})
			requested, _ := store.RunCancelRequested(ctx, owner.TenantID, runID)
			run, _ := store.GetRun(ctx, owner.TenantID, runID)
			ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, askID)
			if requested || run.Status != "running" || ask.Status != "pending" || daemon.coordinator().activeForRun(owner.PersonID, runID) == nil || !strings.Contains(response.Content+response.Error, "injected stop failure") {
				t.Fatalf("failed persistence cancelled live work: response=%+v run=%+v ask=%+v requested=%v", response, run, ask, requested)
			}
			if _, err := raw.Exec(`DROP TRIGGER reject_active_stop`); err != nil {
				t.Fatal(err)
			}
			daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin", Content: "/stop " + runID})
			waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "persisted stop did not unwind")
		})
	}
}
