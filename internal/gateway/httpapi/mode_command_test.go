package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// TestModeCommandPersistsAndResolves proves the /mode entry point: it persists
// the person's approval mode, and a later request with no explicit mode resolves
// to the persisted value (an explicit per-request mode still wins).
func TestModeCommandPersistsAndResolves(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	// Default: on-request when nothing is persisted.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/mode"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "smart") {
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

// recordingJudge is a fake smart-mode triage judge for the live-lookup test.
type recordingJudge struct {
	mu     sync.Mutex
	reply  string
	called int
}

func (j *recordingJudge) Judge(ctx context.Context, prompt string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.called++
	return j.reply, nil
}

func (j *recordingJudge) calls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.called
}

// TestApprovalModeLiveLookupMidRun pins the live-mode contract end to end at
// the gateway layer: the execution scope installed at run start re-resolves
// the person's persisted /mode preference at EACH approval decision, so a
// `/mode` change sent from another endpoint mid-run (here: flipping
// person_settings between two dangerous ops) governs the next ask. An
// explicit per-request mode stays pinned for that run.
func TestApprovalModeLiveLookupMidRun(t *testing.T) {
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
	coord := daemon.coordinator()

	// The run starts with NO explicit request mode and no persisted preference:
	// the snapshot is on-request, exactly the live-defect setup.
	cleanup := coord.installExecutionScope(context.Background(), identity, nil, nil, nil, api.MessageRequest{})
	defer cleanup()

	ran := 0
	exec := tools.SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran++
		return "ran", nil
	})
	dangerousOp := map[string]interface{}{
		"_tenant_id": identity.PersonID,
		"_tool_name": "terminal",
		"command":    "chmod 777 script.sh",
	}

	// Op 1 under a mid-run flip to full-auto: the frozen-snapshot bug would
	// still ask (on-request); the live lookup must bypass.
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "full-auto"); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("full-auto flip must govern the next op: %v", err)
	}
	if ran != 1 || judge.calls() != 0 {
		t.Fatalf("full-auto must bypass ask and triage: ran=%d judge=%d", ran, judge.calls())
	}

	// Op 2 after flipping to smart mid-run: the H2 triage must engage (the
	// judge auto-approves here, so nothing blocks on a human).
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "smart"); err != nil {
		t.Fatal(err)
	}
	// A different dangerous class so the triage-recorded class grant from any
	// earlier step cannot mask the judge consultation.
	if _, err := exec(map[string]interface{}{
		"_tenant_id": identity.PersonID,
		"_tool_name": "terminal",
		"command":    "rm -rf build",
	}); err != nil {
		t.Fatalf("smart triage APPROVE must run the op: %v", err)
	}
	if ran != 2 || judge.calls() != 1 {
		t.Fatalf("smart mid-run must engage triage: ran=%d judge=%d", ran, judge.calls())
	}
	cleanup()

	// An explicit per-request mode stays pinned for the run: persisted flips
	// no longer move it.
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "on-request"); err != nil {
		t.Fatal(err)
	}
	cleanupExplicit := coord.installExecutionScope(context.Background(), identity, nil, nil, nil, api.MessageRequest{ApprovalMode: "full-auto"})
	defer cleanupExplicit()
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("explicit request mode must win over the persisted preference: %v", err)
	}
	if ran != 3 || judge.calls() != 1 {
		t.Fatalf("explicit full-auto must bypass ask and triage: ran=%d judge=%d", ran, judge.calls())
	}
}
