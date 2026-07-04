package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
)

// TestCLIAsyncResultRoutesToPreferredIM encodes the continuity promise for
// fire-and-forget terminal runs: the final answer must reach the person's
// preferred IM endpoint instead of vanishing (observed live: a rejected
// approval's acknowledgment was invisible, so the rejection looked like a
// no-op). With a single bound account, that account is the preferred one.
func TestCLIAsyncResultRoutesToPreferredIM(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.coordinator().deliverAsyncResult(ctx, identity,
		api.MessageRequest{Platform: "cli", Channel: "cli"},
		api.MessageResponse{
			Content: "The user rejected chmod; nothing else was run.",
			Task:    task,
		})

	if len(recorder.messages) != 1 {
		t.Fatalf("messages = %+v", recorder.messages)
	}
	msg := recorder.messages[0]
	if msg.Platform != "weixin" || msg.PlatformUserID != "wxid_123" {
		t.Fatalf("target = %s/%s", msg.Platform, msg.PlatformUserID)
	}
	if !strings.Contains(msg.Content, "rejected chmod") || !strings.Contains(msg.Content, task.Title) {
		t.Fatalf("result content should carry the answer and the task title:\n%s", msg.Content)
	}
}
