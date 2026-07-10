package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestDiagHidesInternalEventAndChannelIdentifiers(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
		t.Fatal(err)
	}
	const channelID = "private-user-id@im.wechat"
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, Type: "agent.thinking", Visibility: "task", Channel: channelID,
	}); err != nil {
		t.Fatal(err)
	}

	handled, reply, err := daemon.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: channelID, Content: "/diag"})
	if !handled || err != nil {
		t.Fatalf("/diag handled=%v err=%v", handled, err)
	}
	if strings.Contains(reply, channelID) || strings.Contains(reply, "agent.thinking") {
		t.Fatalf("/diag leaked internal identifiers:\n%s", reply)
	}
	if !strings.Contains(reply, "AI prepared the next step") {
		t.Fatalf("/diag should retain a user-facing activity summary:\n%s", reply)
	}
}
