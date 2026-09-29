package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestTelegramReplyMetadataUsesPlatformChatAndSender(t *testing.T) {
	req := messageRequestFromIM("telegram", map[string]interface{}{
		"update_id": float64(99),
		"message": map[string]interface{}{
			"text":             "one more condition",
			"from":             map[string]interface{}{"id": float64(13)},
			"chat":             map[string]interface{}{"id": float64(42)},
			"reply_to_message": map[string]interface{}{"message_id": float64(87)},
		},
	})
	if req.PlatformUserID != "13" || req.Channel != "42" || req.NativeReplyMessageID != "87" || req.Content != "one more condition" {
		t.Fatalf("Telegram reply metadata = %+v", req)
	}
}

func TestNativeReplyAnswersExactApprovalInMultiRunChat(t *testing.T) {
	daemon, store, identity, task, first := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "telegram", "user-1", "Owner"); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		ActionType: "tool_call", Payload: first.Payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "telegram",
		PlatformUserID: "user-1", Channel: "chat-1", ApprovalID: second.ID,
		Kind: "approval", Content: "Approve this action?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimDelivery(ctx, d.ID); err != nil || !claimed {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	if err := store.MarkDeliverySentWithNativeID(ctx, d.ID, "87"); err != nil {
		t.Fatal(err)
	}
	req := api.MessageRequest{Platform: "telegram", PlatformUserID: "user-1", Channel: "chat-1",
		Content: "y", NativeReplyMessageID: "87"}
	resp, status := daemon.ProcessMessage(ctx, req)
	if status != http.StatusOK || !strings.Contains(resp.Content, "Approved:") {
		t.Fatalf("exact approval reply: %d %+v", status, resp)
	}
	firstNow, _ := store.GetApprovalRequest(ctx, identity.TenantID, first.ID)
	secondNow, _ := store.GetApprovalRequest(ctx, identity.TenantID, second.ID)
	if firstNow.Status != "pending" || secondNow.Status != "approved" {
		t.Fatalf("approval cross-route: first=%s second=%s", firstNow.Status, secondNow.Status)
	}
	req.PlatformUserID = "stranger"
	resp, status = daemon.ProcessMessage(ctx, req)
	if status != http.StatusOK || !strings.Contains(resp.Content, "not linked") {
		t.Fatalf("foreign reply was not rejected: %d %+v", status, resp)
	}
	req.Content = "/status"
	resp, status = daemon.ProcessMessage(ctx, req)
	if status != http.StatusOK || strings.Contains(resp.Content, "not linked") {
		t.Fatalf("explicit control was shadowed by native reply metadata: %d %+v", status, resp)
	}
}
