package cliapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	gatewayrt "selfmind/internal/runtime/gateway"
)

func TestBuildDoctorReportRendersSectionsAndRedacts(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}

	const secret = "SuperSecretHunter2"

	// Seed one of each section, planting the secret in redactable fields.
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Doctor task", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "do it")
	_ = store.FinishRun(ctx, identity.TenantID, run.ID, "done")

	store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "deploy with password=" + secret})

	payload, _ := json.Marshal(map[string]string{"tool": "terminal", "reason": "run", "cmd": "export token=" + secret})
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		ActionType: "tool_call", Status: "pending", Payload: payload, RequestedChannel: "cli",
	}); err != nil {
		t.Fatalf("approval: %v", err)
	}

	del, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "weixin",
		PlatformUserID: "wxid_x", Channel: "wxid_x", Content: "secret=" + secret + " push",
	})
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	_ = store.MarkDeliverySentUnconfirmed(ctx, del.ID)

	_ = store.RecordChannelMessage(ctx, *identity, "cli", task.ID, "user", "hi")

	// Plant the secret in the gateway log too.
	logPath := gatewayrt.ResolvePaths(dataDir).LogPath
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("boot ok\napi_key="+secret+" in log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report := buildDoctorReport(ctx, store, identity, dataDir, "not running", "", 50)

	for _, section := range []string{
		"== Gateway ==", "== Workspace trust ==", "== Recent runs ==", "== Pending approvals ==",
		"== Queued tasks ==", "== Unconfirmed / failed pushes ==",
		"== Presence (bound accounts) ==", "== Activity by channel ==", "== Gateway log",
	} {
		if !strings.Contains(report, section) {
			t.Errorf("report missing section %q\n---\n%s", section, report)
		}
	}
	// Section content landed.
	if !strings.Contains(report, "Doctor task") {
		t.Errorf("recent runs missing the task title")
	}
	if !strings.Contains(report, "boot ok") {
		t.Errorf("gateway log tail missing")
	}
	// The planted secret must be redacted everywhere.
	if strings.Contains(report, secret) {
		t.Fatalf("doctor report leaked the planted secret:\n%s", report)
	}
}
