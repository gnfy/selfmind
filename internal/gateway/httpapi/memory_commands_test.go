package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

func TestMemoryCommandsRefuseMissingSkillStorageWithoutTouchingHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon, _, identity := newMemoryCommandServer(t)
	daemon.SkillStorage = nil

	_, err := daemon.handleRememberCommand(context.Background(), identity, "回复时先给结论")
	if err == nil || !strings.Contains(err.Error(), "asset storage") {
		t.Fatalf("missing SkillStorage must fail before memory mutation, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".selfmind")); !os.IsNotExist(statErr) {
		t.Fatalf("memory command fell back to the user home: %v", statErr)
	}
}

func newMemoryCommandServer(t *testing.T) (*Server, *memory.MemoryManager, *control.IdentityContext) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	mem := memory.NewMemoryManager(provider)
	storage, err := tools.NewSkillStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, Memory: mem, SkillStorage: storage, DefaultTenantID: "default"}, mem, identity
}

// TestRememberAndForgetAreCrossEndpointAndDeterministic pins the P3 explicit
// path: /remember stores a user-confirmed preference with no model call, the
// preference is readable from ANOTHER channel of the same person, and /forget
// by text removes it from the read model immediately.
func TestRememberAndForgetAreCrossEndpointAndDeterministic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon, mem, identity := newMemoryCommandServer(t)
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "/remember 回复时先给结论再给论据",
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Remembered") {
		t.Fatalf("remember failed: status=%d resp=%+v", status, resp)
	}
	// The preference is person-partitioned and user-confirmed: any endpoint of
	// the same person reads it back.
	facts, _ := memory.ReadModelFacts(ctx, mem, identity.PersonID)
	var stored bool
	for _, f := range facts {
		if strings.Contains(f.Content, "先给结论") && f.Target == "user" {
			stored = true
		}
	}
	if !stored {
		t.Fatalf("preference not stored in person partition: %+v", facts)
	}
	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store missing")
	}
	rows, err := store.ListCanonicalMemories(ctx, identity.PersonID, memory.CanonicalFilter{})
	if err != nil || len(rows) != 1 || !rows[0].UserConfirmed {
		t.Fatalf("remember must write a user-confirmed canonical row: %+v err=%v", rows, err)
	}

	// Forget by TEXT from a different channel of the same person.
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "weixin-session",
		Content: "/forget 先给结论",
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Forgotten") {
		t.Fatalf("forget failed: status=%d resp=%+v", status, resp)
	}
	facts, _ = memory.ReadModelFacts(ctx, mem, identity.PersonID)
	for _, f := range facts {
		if strings.Contains(f.Content, "先给结论") {
			t.Fatalf("forgotten preference still served: %+v", f)
		}
	}
	legacy, err := mem.GetFacts(ctx, identity.PersonID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 {
		t.Fatalf("forgotten preference remains in raw legacy storage: %+v", legacy)
	}
	if _, err := os.Stat(filepath.Join(daemon.SkillStorage.BaseDir(), identity.PersonID, "learning", "learning-log.jsonl")); err != nil {
		t.Fatalf("memory audit was not written to injected asset storage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".selfmind")); !os.IsNotExist(err) {
		t.Fatalf("memory command leaked into HOME: %v", err)
	}
}

// TestRememberRefusesTransientRunState pins the memory floor: explicit or not,
// transient run/build state never enters long-term memory through /remember.
func TestRememberRefusesTransientRunState(t *testing.T) {
	daemon, mem, identity := newMemoryCommandServer(t)
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "/remember Build ID: cw-prod:0d4a9e81 has been created",
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "transient") {
		t.Fatalf("transient remember must be refused with guidance: %+v", resp)
	}
	facts, _ := memory.ReadModelFacts(ctx, mem, identity.PersonID)
	if len(facts) != 0 {
		t.Fatalf("nothing may be stored: %+v", facts)
	}
}

// TestForgetDisambiguatesMultipleMatches: several matches return a numbered
// ref list and forget nothing until the person picks one.
func TestForgetDisambiguatesMultipleMatches(t *testing.T) {
	daemon, mem, identity := newMemoryCommandServer(t)
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	for _, content := range []string{"/remember 回复用中文", "/remember 提交信息用中文写"} {
		if resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: content,
		}); status != http.StatusOK || !strings.Contains(resp.Content, "Remembered") {
			t.Fatalf("seed remember failed: %+v", resp)
		}
	}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/forget 中文",
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Several memories match") {
		t.Fatalf("ambiguous forget must list refs: %+v", resp)
	}
	facts, _ := memory.ReadModelFacts(ctx, mem, identity.PersonID)
	if len(facts) != 2 {
		t.Fatalf("ambiguous forget must not remove anything: %+v", facts)
	}
	// Picking the listed ref resolves it.
	store, _ := mem.Canonical()
	rows, _ := store.ListCanonicalMemories(ctx, identity.PersonID, memory.CanonicalFilter{})
	if len(rows) == 0 {
		t.Fatal("expected canonical rows")
	}
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "/forget " + rows[0].ID[:8],
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Forgotten") {
		t.Fatalf("forget by ref failed: %+v", resp)
	}
}
