package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

// A human wait owns its question and checkpoint, not an idle directory lock.
// Exercise both kinds through the message path and real worker pool.
func TestParkedHumanWaitAdmitsNewWorkInSameWorkspace(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			daemon, store, provider, owner := newHumanWaitServer(t)
			daemon.HumanWaitGrace = 20 * time.Millisecond
			provider.clarification = kind == "clarification"
			ws := humanWaitWorkspace(t, store, owner, "shared")
			startHumanWait(t, daemon, ws, "session-a")
			ctx := context.Background()
			waitUntil(t, time.Second, func() bool {
				if daemon.coordinator().activeCount(owner.PersonID) != 0 {
					return false
				}
				if kind == "approval" {
					asks, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					return len(asks) == 1 && asks[0].WaiterState == "parked"
				}
				asks, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
				return len(asks) == 1 && asks[0].ParkedAt != nil
			}, "first task did not durably park")
			response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-b", WorkspaceID: ws.ID, Content: "independent work", ForceNew: true, Async: true})
			if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.QueueID != "" {
				t.Fatalf("idle human wait queued new work in the same directory: code=%d response=%+v", code, response)
			}
			waitUntil(t, time.Second, func() bool { return provider.independent.Load() == 1 }, "new task did not reach Main")
			// New work must neither answer nor dismiss the original wait.
			if kind == "approval" {
				asks, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
				if len(asks) != 1 {
					t.Fatal("new work consumed the old approval")
				}
			} else {
				asks, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
				if len(asks) != 1 {
					t.Fatal("new work consumed the old question")
				}
			}
		})
	}
}
