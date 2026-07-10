package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// argRecordingTool captures the args it was dispatched with, so tests can
// assert which partition (_tenant_id) the daemon injected.
type argRecordingTool struct {
	name string
	got  map[string]interface{}
}

func (t *argRecordingTool) Name() string        { return t.name }
func (t *argRecordingTool) Description() string { return "test recorder" }
func (t *argRecordingTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{Type: "object"}
}
func (t *argRecordingTool) Execute(args map[string]interface{}) (string, error) {
	t.got = args
	return "ok", nil
}

// TestDispatchPartitionScoping pins the person-partition fix: daemon agent
// runs store memory/sessions under the PERSON id, so dispatched
// person-partitioned tools must receive identity.PersonID as _tenant_id —
// while skill tools keep the control tenant (the daemon's skills dir key).
// Regression: dispatch used to force the control tenant for everything, so a
// client /memory list read an empty partition and could not see what the
// daemon had learned.
func TestDispatchPartitionScoping(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	memTool := &argRecordingTool{name: "memory"}
	skillTool := &argRecordingTool{name: "skill_manage"}
	reg := tools.NewRegistry()
	reg.Register(memTool)
	reg.Register(skillTool)
	disp := tools.NewDispatcherWithRegistry(reg)

	agent := kernel.NewAgent(memory.NewMemoryManager(nil), disp, nil, "test", 1, 1, nil)
	daemon := &Server{
		Control:         store,
		Gateway:         router.NewGateway(nil, nil, agent, nil),
		DefaultTenantID: "default",
	}

	call := func(tool string) {
		t.Helper()
		body, _ := json.Marshal(api.DispatchRequest{
			Tool: tool, Platform: "cli", PlatformUserID: "local",
			Args: map[string]interface{}{"action": "list"},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/dispatch", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("dispatch %s status = %d body=%s", tool, rec.Code, rec.Body.String())
		}
	}

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}

	call("memory")
	if got, _ := memTool.got["_tenant_id"].(string); got != identity.PersonID {
		t.Fatalf("memory dispatched with _tenant_id=%q, want person id %q", got, identity.PersonID)
	}
	call("skill_manage")
	if got, _ := skillTool.got["_tenant_id"].(string); got != identity.TenantID {
		t.Fatalf("skill_manage dispatched with _tenant_id=%q, want control tenant %q", got, identity.TenantID)
	}
}

// fakeSessions is a scripted SessionsBackend that records the partition used.
type fakeSessions struct {
	gotTenant string
	sessions  []memory.FTS5Session
	messages  []memory.SessionMessage
}

func (f *fakeSessions) SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error) {
	f.gotTenant = tenantID
	return f.sessions, nil
}
func (f *fakeSessions) ListRecentSessions(tenantID string, limit int) ([]memory.FTS5Session, error) {
	f.gotTenant = tenantID
	return f.sessions, nil
}
func (f *fakeSessions) GetSessionMessages(tenantID, sessionID string, around, window int) ([]memory.SessionMessage, error) {
	f.gotTenant = tenantID
	return f.messages, nil
}

// TestSessionsEndpoint covers /v1/sessions: search, recent, and the message
// window, all on the PERSON partition.
func TestSessionsEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fake := &fakeSessions{
		sessions: []memory.FTS5Session{{SessionID: "task:t1", Channel: "cli", Summary: "refactor loader", Timestamp: 42}},
		messages: []memory.SessionMessage{{SessionID: "task:t1", MessageID: 7, Role: "user", Content: "hello", Timestamp: 42}},
	}
	daemon := &Server{Control: store, DefaultTenantID: "default", Sessions: fake}

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}

	get := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Search.
	rec := get("/v1/sessions?q=refactor&platform=cli&platform_user_id=local")
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d body=%s", rec.Code, rec.Body.String())
	}
	var sessions api.SessionsResponse
	if err := json.NewDecoder(rec.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].SessionID != "task:t1" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if fake.gotTenant != identity.PersonID {
		t.Fatalf("search partition = %q, want person id %q", fake.gotTenant, identity.PersonID)
	}

	// Recent (no q).
	fake.gotTenant = ""
	if rec := get("/v1/sessions?platform=cli&platform_user_id=local"); rec.Code != http.StatusOK {
		t.Fatalf("recent status = %d", rec.Code)
	}
	if fake.gotTenant != identity.PersonID {
		t.Fatalf("recent partition = %q, want person id", fake.gotTenant)
	}

	// Message window.
	rec = get("/v1/sessions?session_id=task:t1&platform=cli&platform_user_id=local")
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d", rec.Code)
	}
	var msgs api.SessionMessagesResponse
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs.Messages) != 1 || msgs.Messages[0].MessageID != 7 {
		t.Fatalf("messages = %+v", msgs)
	}

	// Nil backend → 503, not a panic.
	daemon.Sessions = nil
	if rec := get("/v1/sessions?q=x&platform=cli&platform_user_id=local"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend status = %d, want 503", rec.Code)
	}
}
