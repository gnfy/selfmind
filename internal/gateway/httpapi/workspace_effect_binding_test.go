package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

// The endpoint registers the whole declared invocation: the directory the
// script runs from, its assignments and its standard input. An invalid
// declaration is refused instead of silently binding a broader invocation.
func TestEffectProfileEndpointBindsTheDeclaredInvocation(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local user")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "deploy.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
		OwnerPersonID: identity.PersonID, Name: "repo", LocalPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID, workspace.ID,
		executionenv.TrustTrusted, "local_cli"); err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store}
	post := func(request api.WorkspaceEffectProfileRequest) *httptest.ResponseRecorder {
		request.Platform, request.PlatformUserID, request.WorkspaceID = "cli", "local", workspace.ID
		request.ScriptPath, request.TargetKeys = "scripts/deploy.sh", []string{"cluster:east"}
		payload, _ := json.Marshal(request)
		httpRequest := httptest.NewRequest(http.MethodPost, "/v1/workspaces/effect-profiles", bytes.NewReader(payload))
		httpRequest.RemoteAddr = "127.0.0.1:4100"
		httpRequest = asLocalCLI(httpRequest)
		response := httptest.NewRecorder()
		server.handleWorkspaceEffectProfiles(response, httpRequest)
		return response
	}
	yes := "yes"
	response := post(api.WorkspaceEffectProfileRequest{Argv: []string{"east"}, Cwd: "scripts",
		Env: []string{"DEPLOY_REGION=eu-1"}, StdinText: &yes})
	if response.Code != http.StatusOK {
		t.Fatalf("declared invocation = %d: %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "run from scripts") ||
		!strings.Contains(body, "1 declared env assignment") || !strings.Contains(body, "declared stdin text") {
		t.Fatalf("registered label does not describe the declared invocation: %s", body)
	}
	if response := post(api.WorkspaceEffectProfileRequest{Cwd: "missing"}); response.Code != http.StatusBadRequest {
		t.Fatalf("a missing cwd = %d: %s", response.Code, response.Body.String())
	}
	if response := post(api.WorkspaceEffectProfileRequest{Env: []string{"REGION"}}); response.Code != http.StatusBadRequest {
		t.Fatalf("an assignment without a value = %d: %s", response.Code, response.Body.String())
	}
}
