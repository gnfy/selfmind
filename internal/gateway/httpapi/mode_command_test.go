package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// TestModeCommandPersistsAndResolves proves the /mode entry point: it persists
// the person's approval mode, and a later request with no explicit mode resolves
// to the persisted value (an explicit per-request mode still wins).
func TestModeCommandPersistsAndResolves(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	// Default: on-request when nothing is persisted.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "on-request") {
		t.Fatalf("/mode default: status=%d content=%q", status, resp.Content)
	}

	// Set smart.
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode smart"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "smart") {
		t.Fatalf("/mode smart: status=%d content=%q", status, resp.Content)
	}
	if got, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode); got != "smart" {
		t.Fatalf("persisted approval_mode = %q, want smart", got)
	}

	// A later request with empty mode resolves to smart.
	if got := daemon.coordinator().resolveApprovalMode(identity, ""); got != tools.ApprovalSmart {
		t.Fatalf("resolved mode with empty request = %q, want smart", got)
	}
	// An explicit per-request mode still wins.
	if got := daemon.coordinator().resolveApprovalMode(identity, "read-only"); got != tools.ApprovalReadOnly {
		t.Fatalf("explicit request mode should win, got %q", got)
	}

	// full-auto warns about the hard floor.
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode full-auto"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "hard-floor") {
		t.Fatalf("/mode full-auto should warn about the hard floor: %q", resp.Content)
	}

	// An unknown mode is rejected and does not overwrite the persisted value.
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode bogus"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Unknown mode") {
		t.Fatalf("/mode bogus should be rejected: %q", resp.Content)
	}
	if got, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode); got != "full-auto" {
		t.Fatalf("unknown mode must not overwrite persisted value; got %q", got)
	}
}
