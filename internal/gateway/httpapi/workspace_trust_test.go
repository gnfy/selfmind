package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

func TestWorkspaceTrustRequiresLocalCLIRequest(t *testing.T) {
	server := &Server{LocalControlToken: "local-secret"}
	for _, test := range []struct {
		name       string
		remoteAddr string
		platform   string
		token      string
		want       bool
	}{
		{name: "ipv4 loopback cli", remoteAddr: "127.0.0.1:4100", platform: "cli", token: "local-secret", want: true},
		{name: "ipv6 loopback cli", remoteAddr: "[::1]:4100", platform: "cli", token: "local-secret", want: true},
		{name: "missing privileged token", remoteAddr: "127.0.0.1:4100", platform: "cli", want: false},
		{name: "forged privileged token", remoteAddr: "127.0.0.1:4100", platform: "cli", token: "wrong", want: false},
		{name: "remote cli", remoteAddr: "203.0.113.10:4100", platform: "cli", token: "local-secret", want: false},
		{name: "local im", remoteAddr: "127.0.0.1:4100", platform: "weixin", token: "local-secret", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/workspaces/trust", nil)
			request.RemoteAddr = test.remoteAddr
			request.Header.Set(api.LocalControlTokenHeader, test.token)
			if got := server.localCLIRequest(request, test.platform); got != test.want {
				t.Fatalf("localCLIRequest(%q, %q) = %v, want %v", test.remoteAddr, test.platform, got, test.want)
			}
		})
	}
}

func TestObservationProfileEndpointRequiresTrustedWorkspaceAndLocalCLI(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local user")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script := filepath.Join(root, "inspect.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.EnsureWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "repo", LocalPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, LocalControlToken: "local-secret"}
	payload, _ := json.Marshal(api.WorkspaceObservationProfileRequest{
		Platform: "cli", PlatformUserID: "local", WorkspaceID: workspace.ID,
		ScriptPath: script, AllowTrailing: true,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/observation-profiles", bytes.NewReader(payload))
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	response := httptest.NewRecorder()
	server.handleWorkspaceObservationProfiles(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("untrusted status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if _, err := store.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID, workspace.ID, executionenv.TrustTrusted, "local_cli"); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/workspaces/observation-profiles", bytes.NewReader(payload))
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	response = httptest.NewRecorder()
	server.handleWorkspaceObservationProfiles(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("trusted status = %d: %s", response.Code, response.Body.String())
	}
	grants, err := store.ListApprovalGrants(ctx, identity.TenantID, identity.PersonID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || !strings.HasPrefix(grants[0].PatternKey, "rule:"+tools.ApprovalRuleKindObservationScript+":") {
		t.Fatalf("observation grants = %+v", grants)
	}
}

func TestWorkspaceTrustEndpointRequiresLocalControlToken(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")

	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open control store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local user")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	workspace, err := store.EnsureWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "repo",
		LocalPath:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	server := &Server{Control: store, LocalControlToken: "local-secret"}
	body, err := json.Marshal(api.WorkspaceTrustRequest{
		TenantID:       identity.TenantID,
		Platform:       "cli",
		PlatformUserID: "local",
		WorkspaceID:    workspace.ID,
		TrustLevel:     executionenv.TrustTrusted,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	for _, test := range []struct {
		name       string
		token      string
		wantStatus int
		wantTrust  string
	}{
		{name: "missing token", wantStatus: http.StatusForbidden, wantTrust: executionenv.TrustUntrusted},
		{name: "wrong token", token: "wrong", wantStatus: http.StatusForbidden, wantTrust: executionenv.TrustUntrusted},
		{name: "authenticated local cli", token: "local-secret", wantStatus: http.StatusOK, wantTrust: executionenv.TrustTrusted},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/trust", bytes.NewReader(body))
			request.RemoteAddr = "127.0.0.1:4100"
			request.Header.Set(api.LocalControlTokenHeader, test.token)
			response := httptest.NewRecorder()

			server.handleWorkspaceTrust(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			got, err := store.GetWorkspace(ctx, identity.TenantID, workspace.ID)
			if err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if got.TrustLevel != test.wantTrust {
				t.Fatalf("trust = %q, want %q", got.TrustLevel, test.wantTrust)
			}
		})
	}
}

func TestWorkspaceCapabilityEndpointListsAndRevokesWithLocalControlToken(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")

	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open control store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local user")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	workspace, err := store.EnsureWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "repo",
		LocalPath:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := store.GrantExecutionCapability(
		ctx,
		identity.TenantID,
		identity.PersonID,
		workspace.ID,
		executionenv.CapabilityNetworkShared,
		"network:shared",
		"local-test",
		time.Now().Add(time.Hour),
	); err != nil {
		t.Fatalf("grant capability: %v", err)
	}

	server := &Server{Control: store, LocalControlToken: "local-secret"}
	query := url.Values{
		"platform":         []string{"cli"},
		"platform_user_id": []string{"local"},
		"workspace_id":     []string{workspace.ID},
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/workspaces/capabilities?"+query.Encode(), nil)
	request.RemoteAddr = "127.0.0.1:4100"
	response := httptest.NewRecorder()
	server.handleWorkspaceCapabilities(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated status = %d, want %d", response.Code, http.StatusForbidden)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/workspaces/capabilities?"+query.Encode(), nil)
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	response = httptest.NewRecorder()
	server.handleWorkspaceCapabilities(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.Code, response.Body.String())
	}
	var listed struct {
		Capabilities []api.WorkspaceCapability `json:"capabilities"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Capabilities) != 1 || listed.Capabilities[0].Capability != executionenv.CapabilityNetworkShared {
		t.Fatalf("capabilities = %+v", listed.Capabilities)
	}

	body, err := json.Marshal(api.WorkspaceCapabilityRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		WorkspaceID:    workspace.ID,
		Capability:     executionenv.CapabilityNetworkShared,
	})
	if err != nil {
		t.Fatalf("marshal revoke: %v", err)
	}
	request = httptest.NewRequest(http.MethodDelete, "/v1/workspaces/capabilities", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:4100"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	response = httptest.NewRecorder()
	server.handleWorkspaceCapabilities(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("revoke status = %d: %s", response.Code, response.Body.String())
	}
	active, err := store.ListActiveExecutionCapabilities(ctx, identity.TenantID, identity.PersonID, workspace.ID)
	if err != nil {
		t.Fatalf("list active after revoke: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active grants after revoke = %+v", active)
	}
}
