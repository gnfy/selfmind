package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
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

// TestModeSmartRetroApprovesPending: a read_file approval is pending (asked
// because it touched a restricted path); switching to smart with a judge that
// returns APPROVE retro-approves it and the reply says so.
func TestModeSmartRetroApprovesPending(t *testing.T) {
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
	judge := &recordingJudge{reply: "APPROVE"}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}

	ap := createPendingApproval(t, store, identity, "read_file", "accesses restricted path: /etc/hosts",
		map[string]interface{}{"path": "/etc/hosts"})

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode smart"})
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(resp.Content, "auto-approved") {
		t.Fatalf("reply should report an auto-approval: %q", resp.Content)
	}
	if got := approvalStatus(t, store, identity, ap.ID); got != "approved" {
		t.Fatalf("pending approval should be approved under smart+APPROVE, got %q", got)
	}
	if judge.calls() != 1 {
		t.Fatalf("smart retro must consult the judge once, got %d", judge.calls())
	}
}

// TestModeSmartRetroEscalateLeavesPending: an ESCALATE judge verdict fails safe
// — the approval stays pending and the reply says a y/n is still needed.
func TestModeSmartRetroEscalateLeavesPending(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	judge := &recordingJudge{reply: "ESCALATE"}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}

	ap := createPendingApproval(t, store, identity, "terminal", "invokes dangerous command: rm",
		map[string]interface{}{"command": "rm -rf build"})

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode smart"})
	if !strings.Contains(resp.Content, "still needs your y/n") {
		t.Fatalf("ESCALATE should leave it pending with a y/n note: %q", resp.Content)
	}
	if got := approvalStatus(t, store, identity, ap.ID); got != "pending" {
		t.Fatalf("ESCALATE must leave the approval pending, got %q", got)
	}
}

// TestModeSmartRetroDenyRejects: a DENY judge verdict rejects the pending
// approval (user-rejection contract) and the reply reports the safety block.
func TestModeSmartRetroDenyRejects(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	judge := &recordingJudge{reply: "DENY"}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}

	ap := createPendingApproval(t, store, identity, "terminal", "invokes dangerous command: chmod",
		map[string]interface{}{"command": "chmod 777 secret"})

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode smart"})
	if !strings.Contains(resp.Content, "blocked by safety triage") {
		t.Fatalf("DENY should report a safety block: %q", resp.Content)
	}
	if got := approvalStatus(t, store, identity, ap.ID); got != "rejected" {
		t.Fatalf("DENY must reject the approval, got %q", got)
	}
}

// TestModeFullAutoRetroApprovesNonHardline: full-auto retro-approves a pending
// non-hardline dangerous op with no judge needed.
func TestModeFullAutoRetroApprovesNonHardline(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	daemon := &Server{Control: store, DefaultTenantID: "default"} // no judge

	ap := createPendingApproval(t, store, identity, "terminal", "invokes dangerous command: rm",
		map[string]interface{}{"command": "rm -rf build"})

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode full-auto"})
	if !strings.Contains(resp.Content, "auto-approved") {
		t.Fatalf("full-auto should auto-approve the pending op: %q", resp.Content)
	}
	if got := approvalStatus(t, store, identity, ap.ID); got != "approved" {
		t.Fatalf("full-auto must approve the non-hardline op, got %q", got)
	}
}

// TestModeFullAutoNeverApprovesHardline: the hard floor is authoritative — a
// hardline pending op is NEVER auto-approved, even by full-auto.
func TestModeFullAutoNeverApprovesHardline(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	// A recursive delete of the filesystem root is a hard-floor op.
	ap := createPendingApproval(t, store, identity, "terminal", "contains dangerous pattern",
		map[string]interface{}{"command": "rm -rf /"})

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode full-auto"})
	if got := approvalStatus(t, store, identity, ap.ID); got != "pending" {
		t.Fatalf("a hardline op must NEVER be auto-approved by any mode, got %q", got)
	}
	if !strings.Contains(resp.Content, "still needs your y/n") {
		t.Fatalf("hardline op should be reported as still pending: %q", resp.Content)
	}
}

// TestModeOnRequestLeavesPending: on-request still asks, so it never
// retro-settles a pending approval and adds no re-check line.
func TestModeOnRequestLeavesPending(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	ap := createPendingApproval(t, store, identity, "terminal", "invokes dangerous command: rm",
		map[string]interface{}{"command": "rm -rf build"})

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode on-request"})
	if strings.Contains(resp.Content, "Re-checked") {
		t.Fatalf("on-request must not retro-resolve anything: %q", resp.Content)
	}
	if got := approvalStatus(t, store, identity, ap.ID); got != "pending" {
		t.Fatalf("on-request must leave the approval pending, got %q", got)
	}
}
