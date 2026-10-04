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

// TestApprovalModeIsFrozenForRun proves a person-level /mode change does not
// expand an already admitted Run's authority. A new Run takes the new setting,
// and an explicit request mode still wins at admission.
func TestApprovalModeIsFrozenForRun(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	judge := &recordingJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"The test user authorized this bounded operation."}`}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}
	coord := daemon.coordinator()
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "full-auto"); err != nil {
		t.Fatal(err)
	}
	cleanup := coord.installExecutionScope(ctx, identity, nil, nil, nil, api.MessageRequest{})
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
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "smart"); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("frozen full-auto run: %v", err)
	}
	if ran != 1 || judge.calls() != 0 {
		t.Fatalf("mode changed inside active Run: ran=%d judge=%d", ran, judge.calls())
	}
	cleanup()

	cleanupNew := coord.installExecutionScope(ctx, identity, nil, nil, nil, api.MessageRequest{})
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("new smart run: %v", err)
	}
	if ran != 2 || judge.calls() != 1 {
		t.Fatalf("new Run ignored updated preference: ran=%d judge=%d", ran, judge.calls())
	}
	cleanupNew()

	cleanupExplicit := coord.installExecutionScope(ctx, identity, nil, nil, nil, api.MessageRequest{ApprovalMode: "full-auto"})
	defer cleanupExplicit()
	if _, err := exec(dangerousOp); err != nil {
		t.Fatalf("explicit full-auto run: %v", err)
	}
	if ran != 3 || judge.calls() != 1 {
		t.Fatalf("explicit mode lost precedence: ran=%d judge=%d", ran, judge.calls())
	}
}
