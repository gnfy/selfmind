package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestWatchersControlCommandListsDetailsAndCancels(t *testing.T) {
	server, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "waiting_external", "waiting", nil); err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Channel: "cli", Description: "RUQX build completion",
		CWD: t.TempDir(), Command: "secret command must not render", SuccessPattern: "SUCCEEDED",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	list := controlReply(t, server, "/watchers")
	for _, want := range []string{"Watchers", "1. [pending] RUQX build completion", shortExternalWatchID(watch.ID), "waiting_external", "Open: /watchers <n|id>"} {
		if !strings.Contains(list, want) {
			t.Fatalf("watcher list missing %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "secret command") {
		t.Fatalf("watch command leaked into list: %s", list)
	}
	detail := controlReply(t, server, "/watchers 1")
	for _, want := range []string{"Status: pending", "Task status: waiting_external", "Cancel: /watchers cancel"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("watcher detail missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "secret command") {
		t.Fatalf("watch command leaked into detail: %s", detail)
	}

	cancel := controlReply(t, server, "/watchers cancel 1")
	if !strings.Contains(cancel, "Cancelled watcher "+shortExternalWatchID(watch.ID)) || !strings.Contains(cancel, "external operation was not cancelled") {
		t.Fatalf("cancel reply = %q", cancel)
	}
	stored, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || stored == nil || stored.Status != control.ExternalWatchCancelled {
		t.Fatalf("stored watch = %+v err=%v", stored, err)
	}
	updated, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || updated.Status != "waiting_user" {
		t.Fatalf("task after cancel = %+v err=%v", updated, err)
	}

	// The command is synchronous control-plane work: it must not be accepted as
	// a model run merely because it arrived through the ordinary message API.
	resp, status := server.ProcessMessage(ctx, api.MessageRequest{
		Platform: identity.Platform, PlatformUserID: identity.PlatformUserID,
		Channel: "cli", Content: "/watchers",
	})
	if status != 200 || resp.Accepted || !strings.Contains(resp.Content, "Watchers") {
		t.Fatalf("message response = %+v status=%d", resp, status)
	}
}

func TestWatcherOrdinalsRoundTripAcrossPages(t *testing.T) {
	server, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	for i := 1; i <= watcherListPageSize+1; i++ {
		if _, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
			Channel: "cli", Description: fmt.Sprintf("watcher-%02d", i), CWD: t.TempDir(),
			Command: "true", SuccessPattern: "ok", TimeoutAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}

	page := controlReply(t, server, "/watchers all 2")
	if !strings.Contains(page, "9. [pending]") {
		t.Fatalf("second page must retain global ordinals:\n%s", page)
	}
	watches, err := store.ListExternalWatchesForPerson(ctx, identity.TenantID, identity.PersonID,
		control.ExternalWatchListAll, 1, watcherListPageSize)
	if err != nil || len(watches) != 1 {
		t.Fatalf("list second-page watcher = %+v err=%v", watches, err)
	}
	detail := controlReply(t, server, "/watchers 9")
	if !strings.Contains(detail, "Watcher "+shortExternalWatchID(watches[0].ID)) {
		t.Fatalf("ordinal 9 did not resolve the watcher printed as 9:\n%s", detail)
	}

	filtered := controlReply(t, server, "/watchers active")
	if strings.Contains(filtered, "1. [") || !strings.Contains(filtered, "- [pending]") {
		t.Fatalf("filtered watcher views must use ids rather than ambiguous local ordinals:\n%s", filtered)
	}
}
