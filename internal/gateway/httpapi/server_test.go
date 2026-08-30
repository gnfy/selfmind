package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

func TestMessageContextBudgetUsesExecutingAgentWindow(t *testing.T) {
	tests := []struct {
		name          string
		contextTokens int
	}{
		{name: "unknown", contextTokens: 0},
		{name: "32k", contextTokens: 32 * 1024},
		{name: "128k", contextTokens: 128 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{}
			if tt.contextTokens > 0 {
				agent := kernel.NewAgent(nil, nil, nil, "test", 1, 1, nil)
				agent.SetContextWindow(tt.contextTokens)
				server.Gateway = router.NewGateway(nil, nil, agent, nil)
			}
			got := server.messageContextBudget(llm.UsageStats{InputTokens: 7, OutputTokens: 3})
			want := kernel.RuntimeContextBudgetForContextTokens(tt.contextTokens)
			if got.SkillMainBytes != want.SkillMainBytes || got.SkillMainTokens != want.SkillMainTokens ||
				got.SkillCatalogBytes != want.SkillCatalogBytes || got.SkillCatalogTokens != want.SkillCatalogTokens {
				t.Fatalf("Skill budget = %+v, want %+v", got, want)
			}
			if got.EstimatedInputTokens != 7 || got.EstimatedOutputTokens != 3 {
				t.Fatalf("usage = %+v", got)
			}
		})
	}
}

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

func TestApprovalsEndpoints(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Approval task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     task.ID,
		ActionType: "shell",
		Payload:    []byte(`{"command":"touch ok"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals?platform=cli&platform_user_id=local", nil)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var listed api.ApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Approvals) != 1 || listed.Approvals[0].ID != approval.ID {
		t.Fatalf("approval list = %+v", listed.Approvals)
	}

	body, _ := json.Marshal(api.ApprovalRespondRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		ApprovalID:     approval.ID,
		Decision:       "approved",
		Channel:        "cli",
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/approvals/respond", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("respond status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var responded api.ApprovalRespondResponse
	if err := json.NewDecoder(rec.Body).Decode(&responded); err != nil {
		t.Fatal(err)
	}
	if responded.Approval == nil || responded.Approval.Status != "approved" {
		t.Fatalf("approval response = %+v", responded.Approval)
	}
	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != "approval.approved" {
		t.Fatalf("approval event missing: %+v", events)
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
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	daemon := &Server{
		Control:         store,
		DefaultTenantID: "default",
		MCPHealthFunc: func() tools.MCPHealthSnapshot {
			return tools.MCPHealthSnapshot{
				Configured: 2,
				Connected:  1,
				Failed:     1,
				Failures:   []tools.MCPServerFailure{{Name: "github", Error: "connection refused"}},
			}
		},
		RuntimeStatusFunc: func() api.GatewayRuntimeInfo {
			return api.GatewayRuntimeInfo{PID: 123, Addr: "127.0.0.1:8765", State: "running"}
		},
		ToolSchemaReportFunc: func() []tools.ToolSchemaReport {
			return []tools.ToolSchemaReport{
				{Name: "read_file", Status: tools.ToolSchemaActive, Exposure: tools.ToolExposureDirect},
				{Name: "skill_view", Status: tools.ToolSchemaRepaired, Exposure: tools.ToolExposureHidden},
				{Name: "mcp_bad", Status: tools.ToolSchemaQuarantined, Exposure: tools.ToolExposureDirect},
			}
		},
		ToolCatalogPreviewFunc: func(context.Context) llm.ToolCatalogPreview {
			return llm.ToolCatalogPreview{Protocol: "openai_chat", Count: 2, Names: []string{"read_file", "finish_run"}, Hash: "catalog123", WireBytes: 512}
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
	if status.Runtime.Version == "" || status.Runtime.BuildFingerprint == "" {
		t.Fatalf("status payload does not expose build identity: %+v", status.Runtime)
	}
	if status.StoreSchema.Version != control.CurrentControlSchemaVersion || status.StoreSchema.CurrentVersion != control.CurrentControlSchemaVersion {
		t.Fatalf("status payload does not expose current control schema: %+v", status.StoreSchema)
	}
	if status.MCP.Configured != 2 || status.MCP.Connected != 1 || status.MCP.Failed != 1 || len(status.MCP.Failures) != 1 {
		t.Fatalf("status payload does not expose MCP health: %+v", status.MCP)
	}
	if !status.ToolCatalog.Valid() || status.ToolCatalog.Hash != "catalog123" || status.ToolCatalog.Count != 2 {
		t.Fatalf("status payload does not expose provider tool catalogue: %+v", status.ToolCatalog)
	}
	if status.ToolSchemas.RegisteredActive != 2 || status.ToolSchemas.Active != 2 || status.ToolSchemas.Hidden != 1 ||
		status.ToolSchemas.ProviderVisible != 2 || status.ToolSchemas.Repaired != 1 || status.ToolSchemas.Quarantined != 1 {
		t.Fatalf("status payload does not distinguish registry, hidden, and provider-visible tools: %+v", status.ToolSchemas)
	}
}

func TestGatewayToolCatalogProbeRequiresLocalControlAndUsesDaemonProbe(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	called := false
	daemon := &Server{
		LocalControlToken: "local-secret",
		ToolCatalogProbeFunc: func(context.Context) api.ProviderToolCatalogProbeResponse {
			called = true
			return api.ProviderToolCatalogProbeResponse{OK: true, Catalog: llm.ToolCatalogPreview{Protocol: "openai_chat", Count: 3}}
		},
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/gateway/tool-catalog/probe", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:4100"
	recorder := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("unauthenticated probe status=%d called=%v", recorder.Code, called)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/gateway/tool-catalog/probe", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !called || !strings.Contains(recorder.Body.String(), `"protocol":"openai_chat"`) {
		t.Fatalf("authenticated probe status=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestGatewayModelProbeRequiresLocalControlAndValidatesRole(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	calledRole := ""
	daemon := &Server{
		LocalControlToken: "local-secret",
		ModelProbeFunc: func(_ context.Context, role string) api.ModelProbeResponse {
			calledRole = role
			return api.ModelProbeResponse{Role: role, OK: true, Provider: "codex-cli", Model: "gpt-test"}
		},
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/probe", strings.NewReader(`{"role":"primary"}`))
	request.RemoteAddr = "127.0.0.1:4100"
	recorder := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || calledRole != "" {
		t.Fatalf("unauthenticated probe status=%d role=%q", recorder.Code, calledRole)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/gateway/model/probe", strings.NewReader(`{"role":"memory_extract"}`))
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || calledRole != "" {
		t.Fatalf("invalid role status=%d role=%q body=%s", recorder.Code, calledRole, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/gateway/model/probe", strings.NewReader(`{"role":"auxiliary"}`))
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calledRole != "auxiliary" || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("authenticated probe status=%d role=%q body=%s", recorder.Code, calledRole, recorder.Body.String())
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

type delayedShutdownResponseWriter struct {
	http.ResponseWriter
	delay time.Duration
}

func (w delayedShutdownResponseWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseWriter.Write(p)
}

func (w delayedShutdownResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w delayedShutdownResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func TestGatewayShutdownResponseSurvivesImmediateServerClose(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	stop := make(chan struct{})
	daemon := &Server{
		DrainTimeout: 100 * time.Millisecond,
		ShutdownFunc: func() {
			close(stop)
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemon.Handler().ServeHTTP(delayedShutdownResponseWriter{
			ResponseWriter: w,
			delay:          25 * time.Millisecond,
		}, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	go func() {
		<-stop
		_ = server.Close()
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("HTTP server did not stop")
		}
	})

	resp, err := http.Post("http://"+listener.Addr().String()+"/v1/gateway/shutdown", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("shutdown request lost its response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown status = %d, want %d", resp.StatusCode, http.StatusAccepted)
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
	if !isControlCommand("/events") {
		t.Fatal("expected /events to be detected")
	}
}

func TestMessageRequestFromIMParsesAttachments(t *testing.T) {
	payload := map[string]interface{}{
		"platform_user_id": "u1",
		"chat_id":          "c1",
		"text":             "inspect this",
		"attachments": []interface{}{
			map[string]interface{}{
				"kind":      "image",
				"path":      "/tmp/selfmind/image.jpg",
				"mime_type": "image/jpeg",
				"name":      "image.jpg",
				"size":      float64(123),
			},
		},
	}
	req := messageRequestFromIM("weixin", payload)
	if len(req.Attachments) != 1 {
		t.Fatalf("attachments = %+v", req.Attachments)
	}
	att := req.Attachments[0]
	if att.Kind != "image" || att.Path != "/tmp/selfmind/image.jpg" || att.MimeType != "image/jpeg" || att.Size != 123 {
		t.Fatalf("attachment = %+v", att)
	}
}

func TestGatewayContextIncludesAttachments(t *testing.T) {
	daemon := &Server{}
	identity := &control.IdentityContext{
		TenantID:       "default",
		PersonID:       "person_1",
		AccountID:      "acct_1",
		Platform:       "weixin",
		PlatformUserID: "wx_user",
	}
	task := &control.Task{ID: "task_1", WorkspaceID: "ws_1"}
	workspace := &control.Workspace{ID: "ws_1", LocalPath: "/repo"}
	content := daemon.coordinator().withGatewayContext("please inspect", identity, task, workspace, nil, []api.MessageAttachment{{
		Kind:     "file",
		Path:     "/tmp/report.pdf",
		MimeType: "application/pdf",
		Name:     "report.pdf",
		Size:     4096,
	}})
	for _, want := range []string{"platform: weixin", "attachments:", "path: /tmp/report.pdf", "mime_type: application/pdf", "inspect attachment paths"} {
		if !strings.Contains(content, want) {
			t.Fatalf("context missing %q:\n%s", want, content)
		}
	}
}

func TestPrepareRequestWorkspaceRegistersCLICWD(t *testing.T) {
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
	cwd := t.TempDir()
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		ClientCWD:      cwd,
	}

	ws, err := daemon.coordinator().prepareRequestWorkspace(ctx, identity, &req)
	if err != nil {
		t.Fatal(err)
	}
	if ws == nil || ws.LocalPath != cwd || req.WorkspaceID == "" {
		t.Fatalf("workspace = %+v, req = %+v", ws, req)
	}
	current, err := store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.ID != ws.ID {
		t.Fatalf("current workspace = %+v, want %s", current, ws.ID)
	}
}

func TestPrepareRequestWorkspaceIgnoresIMClientCWD(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx_user", "Weixin User")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := api.MessageRequest{
		Platform:       "weixin",
		PlatformUserID: "wx_user",
		Channel:        "wx_chat",
		ClientCWD:      t.TempDir(),
	}

	ws, err := daemon.coordinator().prepareRequestWorkspace(ctx, identity, &req)
	if err != nil {
		t.Fatal(err)
	}
	if ws != nil || req.WorkspaceID != "" {
		t.Fatalf("IM should not register client cwd: ws=%+v req=%+v", ws, req)
	}
	workspaces, err := store.ListWorkspaces(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("unexpected workspaces: %+v", workspaces)
	}
}

func TestPrepareRequestWorkspaceUsesCurrentWorkspaceForIM(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx_user", "Weixin User")
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := t.TempDir()
	oldWS, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "old", LocalPath: oldRoot, AllowedRoots: []string{oldRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	newRoot := t.TempDir()
	newWS, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "current", LocalPath: newRoot, AllowedRoots: []string{newRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, newWS.ID); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		WorkspaceID: oldWS.ID, Title: "work in the old workspace", Channel: "weixin",
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx_user", Channel: "wx_chat",
		ClientCWD: t.TempDir(), Content: "inspect this workspace",
	}
	resolved, err := daemon.coordinator().prepareRequestWorkspace(ctx, identity, &req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != newWS.ID || req.WorkspaceID != newWS.ID {
		t.Fatalf("IM request workspace = %+v, request=%+v; want current %s", resolved, req, newWS.ID)
	}
	executionWS, err := daemon.coordinator().workspaceForTask(ctx, identity, task, req, taskAttach{preLabel: true})
	if err != nil {
		t.Fatal(err)
	}
	if executionWS == nil || executionWS.ID != newWS.ID {
		t.Fatalf("pre-label execution workspace = %+v, want current %s", executionWS, newWS.ID)
	}
}

// TestResolveTaskBindsEmptyCurrentTaskToCLIWorkspace: attaching (continuation)
// to a workspace-less task binds the request's resolved CLI workspace to it;
// plain new work pre-labels onto the open current task while executing in the
// request's workspace (Work Timeline P3).
func TestResolveTaskBindsEmptyCurrentTaskToCLIWorkspace(t *testing.T) {
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Existing task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.WorkspaceID != "" {
		t.Fatalf("test setup expected empty workspace: %+v", task)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		ClientCWD:      t.TempDir(),
		Content:        "inspect current project",
	}
	if _, err := daemon.coordinator().prepareRequestWorkspace(ctx, identity, &req); err != nil {
		t.Fatal(err)
	}
	// Continuation evidence attaches to the workspace-less current task and
	// binds the request's resolved CLI workspace to it.
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, req, router.IntentResult{Intent: router.IntentContinue})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != task.ID || resolved.WorkspaceID != req.WorkspaceID {
		t.Fatalf("resolved task = %+v, req workspace = %s", resolved, req.WorkspaceID)
	}
	if attach.preLabel || attach.created {
		t.Fatalf("continuation attach must be explicit, got %+v", attach)
	}
	// Plain new work pre-labels onto the open current task (Work Timeline P3)
	// — a display guess — while the EXECUTION workspace still follows the
	// request (workspaceForTask), so the guess is harmless.
	fresh, attach, err := daemon.coordinator().resolveTask(ctx, identity, req, router.IntentResult{Intent: router.IntentTask})
	if err != nil {
		t.Fatal(err)
	}
	if fresh == nil || fresh.ID != task.ID {
		t.Fatalf("plain work should pre-label onto the open current task, got %+v", fresh)
	}
	if !attach.preLabel || attach.created {
		t.Fatalf("pre-label reuse must be flagged preLabel (not created), got %+v", attach)
	}
	ws, err := daemon.coordinator().workspaceForTask(ctx, identity, fresh, req, attach)
	if err != nil {
		t.Fatal(err)
	}
	if ws == nil || ws.ID != req.WorkspaceID {
		t.Fatalf("pre-label run must execute in the REQUEST workspace %s, got %+v", req.WorkspaceID, ws)
	}
}

func addActiveTaskReference(t *testing.T, store *control.Store, task *control.Task, value string) {
	t.Helper()
	if _, err := store.UpsertTaskReference(context.Background(), control.TaskReferenceWrite{
		TenantID: task.TenantID, PersonID: task.PersonID, TaskID: task.ID, WorkspaceID: task.WorkspaceID,
		Class: control.TaskReferenceLiteral, Value: value, Status: control.TaskReferenceActive,
		UserConfirmed: true, Provenance: "user_control", SourceRef: "test",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTaskUsesActiveReferenceBeforeCurrentPreLabel(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-work-key", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-100 old release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-369 GCP release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, current.ID); err != nil {
		t.Fatal(err)
	}
	addActiveTaskReference(t, store, target, "RUQX-369")

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-work-key", Channel: "cli",
		Content: "check whether RUQX-369 finished",
	}, router.IntentResult{Intent: router.IntentTask})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != target.ID {
		t.Fatalf("explicit work key resolved to %+v, want %s", resolved, target.ID)
	}
	if !attach.preLabel || attach.created || attach.claimsPriorRuns() {
		t.Fatalf("an existing work-key attach must stay display-only: %+v", attach)
	}
	if attach.workKey != "RUQX-369" {
		t.Fatalf("work key=%q want RUQX-369", attach.workKey)
	}
}

func TestResolveTaskUsesActiveReferenceBeforeImplicitContinuation(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-work-key-continue", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-100 old release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-511 GCP release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, current.ID); err != nil {
		t.Fatal(err)
	}
	addActiveTaskReference(t, store, target, "RUQX-511")

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-work-key-continue", Channel: "cli",
		Content: "continue checking RUQX-511",
	}, router.IntentResult{Intent: router.IntentContinue})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != target.ID {
		t.Fatalf("explicit work key resolved to %+v, want %s", resolved, target.ID)
	}
	if attach.preLabel || attach.claimsPriorRuns() || attach.workKey != "RUQX-511" {
		t.Fatalf("reference continuation must remain non-authoritative: %+v", attach)
	}
}

func TestReferenceContinuationCannotChangeExecutionWorkspace(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "reference-scope", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	requestRoot, taskRoot := t.TempDir(), t.TempDir()
	requestWS, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "request", LocalPath: requestRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskWS, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "task", LocalPath: taskRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: taskWS.ID,
		Title: "Customer portal", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	addActiveTaskReference(t, store, target, "customer portal")
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := api.MessageRequest{Platform: "cli", PlatformUserID: "reference-scope", Channel: "cli",
		WorkspaceID: requestWS.ID, Content: "continue customer portal"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, req,
		router.IntentResult{Intent: router.IntentContinue})
	if err != nil || resolved == nil || resolved.ID != target.ID {
		t.Fatalf("resolved=%+v attach=%+v err=%v", resolved, attach, err)
	}
	if attach.claimsPriorRuns() {
		t.Fatalf("semantic reference claimed prior runs: %+v", attach)
	}
	executionWS, err := daemon.coordinator().workspaceForTask(ctx, identity, resolved, req, attach)
	if err != nil || executionWS == nil || executionWS.ID != requestWS.ID {
		t.Fatalf("reference changed execution workspace: got=%+v want=%s err=%v", executionWS, requestWS.ID, err)
	}
}

func TestArchivedReferenceFallsBackToOpenContinuation(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "archived-reference", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	openTask, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Current work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Old portal", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, archived.ID, "archived", "", nil); err != nil {
		t.Fatal(err)
	}
	addActiveTaskReference(t, store, archived, "old portal")
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, openTask.ID); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "archived-reference", Channel: "cli", Content: "continue old portal",
	}, router.IntentResult{Intent: router.IntentContinue})
	if err != nil || resolved == nil || resolved.ID != openTask.ID {
		t.Fatalf("archived reference blocked ordinary continuation: task=%+v attach=%+v err=%v", resolved, attach, err)
	}
	if len(attach.candidateTaskIDs) != 1 || attach.candidateTaskIDs[0] != archived.ID {
		t.Fatalf("unavailable candidate audit lost: %+v", attach)
	}
}

func TestReferenceAmbiguityListsRoutableTasks(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "reference-ambiguity", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Alpha rollout", Channel: "cli"})
	second, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Beta rollout", Channel: "cli"})
	addActiveTaskReference(t, store, first, "alpha rollout")
	addActiveTaskReference(t, store, second, "beta rollout")
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	_, _, err = daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "reference-ambiguity", Channel: "cli",
		Content: "continue alpha rollout and beta rollout",
	}, router.IntentResult{Intent: router.IntentContinue})
	if err == nil || !strings.Contains(err.Error(), shortTaskID(first.ID)) || !strings.Contains(err.Error(), shortTaskID(second.ID)) {
		t.Fatalf("ambiguity did not identify both candidates: %v", err)
	}
}

func TestResolveContinuationDoesNotDeriveReferenceFromTaskTitle(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-continuation-key", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-371 release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-continuation-key", Channel: "cli", Content: "continue",
	}, router.IntentResult{Intent: router.IntentContinue})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != task.ID || !attach.claimsPriorRuns() || attach.workKey != "" {
		t.Fatalf("resolved=%+v attach=%+v", resolved, attach)
	}
}

func TestExplicitTaskDetailedMessageDoesNotClaimPriorWork(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-explicit-detail", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-381 mixed work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-explicit-detail", Channel: "cli",
		TaskID: task.ID, Content: "发布 RUQX-381 的 GCP 服务",
	}, router.IntentResult{Intent: router.IntentTask})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != task.ID || attach.claimsPriorRuns() {
		t.Fatalf("detailed explicit attach must select the label without claiming old work: resolved=%+v attach=%+v", resolved, attach)
	}
}

func TestResumePinDetailedMessageDoesNotClaimPriorWork(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-pin-detail", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-381 mixed work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey, task.ID); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-pin-detail", Channel: "cli",
		Content: "发布 RUQX-381 的另一个 GCP 服务",
	}, router.IntentResult{Intent: router.IntentTask})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != task.ID || attach.claimsPriorRuns() {
		t.Fatalf("detailed resume-pin attach must not claim old work: resolved=%+v attach=%+v", resolved, attach)
	}
}

func TestResolveTaskTreatsUnregisteredWorkKeyAsMetadata(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-new-work-key", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-100 old release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, old.ID); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resolved, attach, err := daemon.coordinator().resolveTask(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-new-work-key", Channel: "cli",
		Content: "prepare RUQX-370 for release",
	}, router.IntentResult{Intent: router.IntentTask})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != old.ID {
		t.Fatalf("an unregistered token must not select or create a task at ingress: old=%s resolved=%+v", old.ID, resolved)
	}
	if !attach.preLabel || attach.created || attach.claimsPriorRuns() || attach.workKey != "RUQX-370" {
		t.Fatalf("unregistered work-key metadata = %+v", attach)
	}
}

func TestModelCommandDoesNotCreateTask(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(httptest.NewRequest(http.MethodPost, "/", nil).Context(), api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		Content:        "/model",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, resp = %+v", status, resp)
	}
	if resp.Error != "" || !strings.Contains(resp.Content, "model gateway is not configured") {
		t.Fatalf("resp = %+v", resp)
	}
	current, err := store.CurrentTask(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "default", resp.Identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("/model command created task: %+v", current)
	}
}

func TestUnresolvedPastePlaceholderIsRejectedBeforeDispatch(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		Content:        "inspect this\n[Paste #1 · 80 lines]",
	})
	if status != http.StatusBadRequest || !strings.Contains(resp.Error, "not expanded") {
		t.Fatalf("status=%d response=%+v; want unresolved-paste rejection", status, resp)
	}
	if resp.Identity != nil || resp.Accepted {
		t.Fatalf("unresolved paste reached dispatch: %+v", resp)
	}
}

func TestContinueWithoutTaskReturnsUserMessage(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(httptest.NewRequest(http.MethodPost, "/", nil).Context(), api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		Content:        "\u7ee7\u7eed",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, resp = %+v", status, resp)
	}
	if resp.Error != "" || !strings.Contains(resp.Content, "no task to continue") {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestResumeContextIncludesLatestHandoff(t *testing.T) {
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Continue work",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{
		TaskID:       task.ID,
		Summary:      "patched gateway",
		DoneItems:    []string{"wired store"},
		NextSteps:    []string{"run tests"},
		ChangedFiles: []string{"internal/gateway/httpapi/server.go"},
	}); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	content := daemon.coordinator().withResumeContext(ctx, identity, task, nil, router.IntentResult{Intent: router.IntentContinue, Confidence: 0.9, Reason: "test"}, false, "", "continue")
	for _, want := range []string{"[SelfMind resume context]", "patched gateway", "wired store", "run tests", "internal/gateway/httpapi/server.go"} {
		if !strings.Contains(content, want) {
			t.Fatalf("resume context missing %q:\n%s", want, content)
		}
	}
	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != "run.resumed" {
		t.Fatalf("run.resumed event missing: %+v", events)
	}
}

func TestExplicitTaskAttachResumesPriorRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Explicit resume", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.StartRun(ctx, task, "cli", "first attempt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity: *identity, RunID: prior.ID, RunStatus: "interrupted",
		TaskID: task.ID, TaskStatus: "interrupted", Summary: "needs continuation",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{TaskID: task.ID, Summary: "resume me"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.StartRun(ctx, task, "cli", "caller supplied task id")
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	_ = daemon.coordinator().withResumeContext(ctx, identity, task, current, router.IntentResult{Intent: router.IntentTask}, true, "", "continue explicitly")
	if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity: *identity, RunID: current.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "done" {
		t.Fatalf("task status=%q, want done after explicit resume", stored.Status)
	}
}

// TestResumeContextIncludesCreatedFilesFromEvents proves a task interrupted
// before any handoff (the codex-EOF case) still carries the files its prior run
// created, recovered from write_file/patch tool events, so the continuation
// edits the right file instead of rediscovering and guessing.
func TestResumeContextIncludesCreatedFilesFromEvents(t *testing.T) {
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Build the KOF 97 game",
		Channel:  "wechat",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A write_file the prior (interrupted) run performed — no handoff was saved.
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		Type:       "tool.started",
		Visibility: "task",
		Channel:    "wechat",
		Payload:    mustJSON(map[string]interface{}{"tool": "write_file", "args": `{"path":"arcade-fury-97.html","content":"<html>...</html>"}`}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		Type:       "tool.completed",
		Visibility: "task",
		Channel:    "wechat",
		Payload:    mustJSON(map[string]interface{}{"tool": "write_file", "result": "Created arcade-fury-97.html (+120 -0)"}),
	}); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	content := daemon.coordinator().withResumeContext(ctx, identity, task, nil, router.IntentResult{Intent: router.IntentContinue, Confidence: 0.9, Reason: "test"}, false, "", "继续")
	for _, want := range []string{"files_this_task_created_or_changed", "arcade-fury-97.html", "Edit these existing files directly"} {
		if !strings.Contains(content, want) {
			t.Fatalf("resume context missing %q:\n%s", want, content)
		}
	}
}

// TestWorkspacePreservedOnResume proves a continuation runs under the task's own
// workspace even when the request carries a different (client cwd-derived)
// workspace — otherwise a CLI `继续` of an IM task would run in the terminal's
// directory and trip out-of-root approvals.
func TestWorkspacePreservedOnResume(t *testing.T) {
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
	taskWS, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "game",
		LocalPath:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clientWS, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "terminal-cwd",
		LocalPath:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: taskWS.ID,
		Title:       "Build the game",
		Channel:     "wechat",
	})
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	// The CLI request carries its own cwd-derived workspace, but the resume must
	// keep the task's original one.
	got, err := daemon.coordinator().workspaceForTask(ctx, identity, task, api.MessageRequest{WorkspaceID: clientWS.ID}, taskAttach{})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != taskWS.ID {
		t.Fatalf("resume workspace = %+v, want task workspace %s", got, taskWS.ID)
	}
}

// TestResumeChangedFilesHarvestsPatchAndEditPaths proves the resume manifest
// covers V4A patch/apply_patch headers and edit-tool file_path args, not just
// write_file — so a continuation of a run that used any file-mutating tool still
// knows the exact paths to edit.
func TestResumeChangedFilesHarvestsPatchAndEditPaths(t *testing.T) {
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Refactor the module",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []control.Event{
		{Type: "tool.started", Payload: mustJSON(map[string]interface{}{"tool": "patch", "args": `{"patch":"*** Begin Patch\n*** Update File: internal/foo.go\n*** End Patch"}`})},
		{Type: "tool.started", Payload: mustJSON(map[string]interface{}{"tool": "apply_patch", "args": `{"patch":"*** Begin Patch\n*** Add File: internal/bar.go\n*** End Patch"}`})},
		{Type: "tool.started", Payload: mustJSON(map[string]interface{}{"tool": "edit", "args": `{"file_path":"internal/baz.go"}`})},
	}
	for _, ev := range events {
		ev.TaskID = task.ID
		ev.Visibility = "task"
		ev.Channel = "cli"
		if _, err := store.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	got := daemon.coordinator().resumeChangedFiles(ctx, task, nil, 10)
	want := map[string]bool{"internal/foo.go": false, "internal/bar.go": false, "internal/baz.go": false}
	for _, p := range got {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Fatalf("resume manifest missing %q, got %v", p, got)
		}
	}
}

func TestTaskEventsEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Events",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "tool.started", Payload: mustJSON(map[string]string{"tool": "read_file"})}); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/events?platform=cli&platform_user_id=local", nil)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Events []control.Event `json:"events"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Events) != 1 || payload.Events[0].Type != "tool.started" {
		t.Fatalf("events payload = %+v", payload.Events)
	}
}

func TestTaskEventsStreamEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Stream events",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "plan.updated", Payload: mustJSON(map[string]interface{}{"plan": []string{"A"}})}); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/events/stream?platform=cli&platform_user_id=local&once=true", nil)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: ready") || !strings.Contains(body, "event: plan.updated") {
		t.Fatalf("unexpected SSE body: %s", body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
}

func TestTaskArtifactsEndpoint(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Artifacts",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveArtifact(ctx, control.Artifact{TaskID: task.ID, Kind: "file", Name: "report.md", URI: "docs/report.md"}); err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/artifacts?platform=cli&platform_user_id=local", nil)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Artifacts []control.Artifact `json:"artifacts"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Artifacts) != 1 || payload.Artifacts[0].URI != "docs/report.md" {
		t.Fatalf("artifacts payload = %+v", payload.Artifacts)
	}
}

func TestRecordOutcomeArtifactsCreatesEvents(t *testing.T) {
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
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Outcome artifacts",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "work")
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	daemon.coordinator().recordOutcomeArtifacts(ctx, task, run, "cli", []string{"internal/app/tools.go", "https://example.com/report"})

	artifacts, err := store.ListTaskArtifacts(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == "artifact.created" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("artifact.created events = %d, events=%+v", count, events)
	}
}

func TestInferTaskStatusConservative(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "clear done", content: "All done - tests pass.", want: "done"},
		{name: "clear chinese done", content: "\u4efb\u52a1\u5df2\u5b8c\u6210", want: "done"},
		{name: "plain success wording", content: "The success criteria are listed below.", want: "running"},
		{name: "not done", content: "Not done yet; remaining work is listed below.", want: "running"},
		{name: "chinese not done", content: "\u8fd8\u6ca1\u5b8c\u6210\uff0c\u9700\u8981\u7ee7\u7eed", want: "running"},
		{name: "blocked", content: "Blocked: need your input before I can continue.", want: "blocked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferTaskStatus(tt.content); got != tt.want {
				t.Fatalf("inferTaskStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildRunOutcomeExtractsStatusAndDetails(t *testing.T) {
	content := `Implementation complete.

Done:
- Updated internal/gateway/httpapi/server.go
- Added internal/gateway/httpapi/outcome.go

Tests:
- go test ./... passed

Next steps:
- Wire IM approval buttons

Risks:
- Feishu signing is not implemented yet
`
	outcome := buildRunOutcome(content)
	if outcome.Status != "running" {
		t.Fatalf("status = %q", outcome.Status)
	}
	if len(outcome.NextSteps) != 1 || outcome.NextSteps[0] != "Wire IM approval buttons" {
		t.Fatalf("next steps = %+v", outcome.NextSteps)
	}
	if len(outcome.Tests) == 0 || !strings.Contains(outcome.Tests[0], "go test") {
		t.Fatalf("tests = %+v", outcome.Tests)
	}
	if len(outcome.Files) < 2 {
		t.Fatalf("files = %+v", outcome.Files)
	}
	if len(outcome.Risks) != 1 {
		t.Fatalf("risks = %+v", outcome.Risks)
	}
}
