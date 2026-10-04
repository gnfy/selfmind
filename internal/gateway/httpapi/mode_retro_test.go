package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

// createPendingApproval inserts a pending tool_call approval whose payload
// matches what toolApprovalHandler writes (tool/reason/args), so the retro
// classifier decodes it exactly like a live one.
func createPendingApproval(t *testing.T, store *control.Store, identity *control.IdentityContext, tool, reason string, args map[string]interface{}) *control.ApprovalRequest {
	t.Helper()
	payload, _ := json.Marshal(map[string]interface{}{"tool": tool, "reason": reason, "args": args})
	ap, err := store.CreateApprovalRequest(context.Background(), control.ApprovalRequest{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		ActionType: "tool_call",
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}
	return ap
}

func approvalStatus(t *testing.T, store *control.Store, identity *control.IdentityContext, id string) string {
	t.Helper()
	got, err := store.GetApprovalRequest(context.Background(), identity.TenantID, id)
	if err != nil || got == nil {
		t.Fatalf("GetApprovalRequest(%s): %v", id, err)
	}
	return got.Status
}

// TestModeChangeKeepsPendingRunAuthority verifies that a person-level setting
// change cannot silently approve or reject an ask belonging to an older Run.
func TestModeChangeKeepsPendingRunAuthority(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	judge := &recordingJudge{reply: "APPROVE"}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}
	approval := createPendingApproval(t, store, identity, "terminal", "invokes dangerous command: chmod",
		map[string]interface{}{"command": "chmod 777 script.sh"})
	for _, mode := range []string{"full-auto", "smart", "auto-edit", "read-only", "on-request"} {
		resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode " + mode})
		if status != http.StatusOK || !strings.Contains(resp.Content, "for new Runs") ||
			!strings.Contains(resp.Content, "/approve or /reject") {
			t.Fatalf("/mode %s: status=%d reply=%q", mode, status, resp.Content)
		}
		if got := approvalStatus(t, store, identity, approval.ID); got != "pending" {
			t.Fatalf("/mode %s changed an existing approval to %s", mode, got)
		}
	}
	if judge.calls() != 0 {
		t.Fatalf("mode change ran a judge for existing work: %d", judge.calls())
	}
}
