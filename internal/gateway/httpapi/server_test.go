package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestMessageRequestFromFeishuEvent(t *testing.T) {
	payload := map[string]interface{}{
		"tenant_id": "acme",
		"event": map[string]interface{}{
			"sender": map[string]interface{}{
				"sender_id": map[string]interface{}{
					"open_id": "ou_123",
				},
			},
			"message": map[string]interface{}{
				"chat_id": "oc_456",
				"content": `{"text":"查一下任务进度"}`,
			},
		},
	}

	req := messageRequestFromIM("feishu", payload)
	if req.TenantID != "acme" {
		t.Fatalf("tenant = %q", req.TenantID)
	}
	if req.Platform != "feishu" {
		t.Fatalf("platform = %q", req.Platform)
	}
	if req.PlatformUserID != "ou_123" {
		t.Fatalf("platform user = %q", req.PlatformUserID)
	}
	if req.Channel != "oc_456" {
		t.Fatalf("channel = %q", req.Channel)
	}
	if req.Content != "查一下任务进度" {
		t.Fatalf("content = %q", req.Content)
	}
}

func TestAccountBindEndpoint(t *testing.T) {
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	body, _ := json.Marshal(api.BindAccountRequest{
		PersonID:       identity.PersonID,
		Platform:       "feishu",
		PlatformUserID: "ou_abc",
		DisplayName:    "Feishu User",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/accounts/bind", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	resolved, err := store.ResolveOrCreateAccount(ctx, "default", "feishu", "ou_abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PersonID != identity.PersonID {
		t.Fatalf("bound person = %q, want %q", resolved.PersonID, identity.PersonID)
	}
}

func TestLegacyDaemonTokenAuth(t *testing.T) {
	t.Setenv("SELF_DAEMON_TOKEN", "secret")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	body := bytes.NewReader([]byte(`{"platform":"cli","platform_user_id":"local","content":"/tasks"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/message", body)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/message", bytes.NewReader([]byte(`{"platform":"cli","platform_user_id":"local","content":"/tasks"}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with token = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGatewayTokenTakesPrecedence(t *testing.T) {
	t.Setenv("SELF_DAEMON_TOKEN", "old")
	t.Setenv("SELF_GATEWAY_TOKEN", "new")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	body := []byte(`{"platform":"cli","platform_user_id":"local","content":"/tasks"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/message", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer old")
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status with old token = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/message", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer new")
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with gateway token = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGatewayStatusEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	daemon := &Server{
		DefaultTenantID: "default",
		RuntimeStatusFunc: func() api.GatewayRuntimeInfo {
			return api.GatewayRuntimeInfo{PID: 123, Addr: "127.0.0.1:8765", State: "running"}
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/gateway/status", nil)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var status api.GatewayStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.State != "running" || status.Runtime.PID != 123 {
		t.Fatalf("status payload = %+v", status)
	}
}

func TestGatewayShutdownEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	stopped := make(chan struct{})
	daemon := &Server{
		DefaultTenantID: "default",
		DrainTimeout:    100 * time.Millisecond,
		ShutdownFunc: func() {
			close(stopped)
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/shutdown", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
	if !daemon.IsDraining() {
		t.Fatal("daemon should be draining after shutdown request")
	}
}

func TestIMAsyncFlagAndControlDetection(t *testing.T) {
	payload := map[string]interface{}{
		"platform_user_id": "u1",
		"chat_id":          "c1",
		"text":             "run a long task",
		"async":            "true",
	}
	req := messageRequestFromIM("webhook", payload)
	if !req.Async {
		t.Fatal("expected async flag to be parsed")
	}
	if isControlCommand(req.Content) {
		t.Fatal("ordinary task should not be treated as a control command")
	}
	if !isControlCommand("/status") || !isControlCommand("/stop") || !isControlCommand("/workspace ws_1") {
		t.Fatal("expected common daemon commands to be detected")
	}
}
