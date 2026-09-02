package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func TestSetDeliveryTargetUsesServerIssuedLiveInput(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	_, _ = store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-bound", "Bound")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "release")
	steering, _ := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID,
		Platform: "weixin", PlatformUserID: "wx-bound", Channel: "wx-chat", Content: "send the result here",
	})
	result, err := NewSetDeliveryTargetTool(store).Execute(map[string]interface{}{
		"input_id": steering.ID,
		"_context": ctx,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		},
	})
	if err != nil || !strings.Contains(result, `"status":"updated"`) || !strings.Contains(result, `"platform":"weixin"`) {
		t.Fatalf("result=%s err=%v", result, err)
	}
	if _, err := NewSetDeliveryTargetTool(store).Execute(map[string]interface{}{
		"input_id":          "steer_invented",
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID},
	}); err == nil {
		t.Fatal("invented input id must not select a destination")
	}
}
