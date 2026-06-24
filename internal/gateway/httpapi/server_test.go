package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
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
	content := daemon.withGatewayContext("please inspect", identity, task, workspace, []api.MessageAttachment{{
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

	ws, err := daemon.prepareRequestWorkspace(ctx, identity, &req)
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

	ws, err := daemon.prepareRequestWorkspace(ctx, identity, &req)
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
	if _, err := daemon.prepareRequestWorkspace(ctx, identity, &req); err != nil {
		t.Fatal(err)
	}
	resolved, err := daemon.resolveTask(ctx, identity, req, router.IntentResult{Intent: router.IntentTask})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != task.ID || resolved.WorkspaceID != req.WorkspaceID {
		t.Fatalf("resolved task = %+v, req workspace = %s", resolved, req.WorkspaceID)
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
	content := daemon.withResumeContext(ctx, identity, task, nil, router.IntentResult{Intent: router.IntentContinue, Confidence: 0.9, Reason: "test"}, "continue")
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
	daemon.recordOutcomeArtifacts(ctx, task, run, "cli", []string{"internal/app/tools.go", "https://example.com/report"})

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
