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
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

func TestMessageEndpointRejectsAddDirWithoutLoopbackControlAuth(t *testing.T) {
	body, err := json.Marshal(api.MessageRequest{
		Platform: "cli", Content: "inspect", ClientAdditionalRoots: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/message", bytes.NewReader(body))
	request.RemoteAddr = "203.0.113.9:3000"
	request.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	(&Server{LocalControlToken: "local-secret"}).Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "authenticated local CLI") {
		t.Fatalf("actionable rejection missing: %s", recorder.Body.String())
	}
}

func TestPrepareRequestExecutionRootsRequiresLocalAuthority(t *testing.T) {
	req := api.MessageRequest{ClientAdditionalRoots: []string{t.TempDir()}}
	err := (&RunCoordinator{}).prepareRequestExecutionRoots(context.Background(), nil, &req)
	if err == nil {
		t.Fatal("remote request was allowed to grant a daemon-host directory")
	}
}

func TestPrepareRequestExecutionRootsCanonicalizesAndFreezes(t *testing.T) {
	workspaceRoot := t.TempDir()
	additionalRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(additionalRoot, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workspace := &control.Workspace{ID: "ws_test", LocalPath: workspaceRoot}
	req := api.MessageRequest{ClientAdditionalRoots: []string{alias}}
	err := (&RunCoordinator{}).prepareRequestExecutionRoots(withLocalFilesystemAuthority(context.Background()), workspace, &req)
	if err != nil {
		t.Fatal(err)
	}
	if !req.AdditionalRootsRequested || len(req.ClientAdditionalRoots) != 0 {
		t.Fatalf("request marker=%v wire roots=%#v", req.AdditionalRootsRequested, req.ClientAdditionalRoots)
	}
	if len(req.ExecutionRoots) != 2 {
		t.Fatalf("bindings = %#v", req.ExecutionRoots)
	}
	if req.ExecutionRoots[0].Path != canonicalStoredDirectory(workspaceRoot) || req.ExecutionRoots[0].Role != executionenv.RootRolePrimary {
		t.Fatalf("primary binding = %#v", req.ExecutionRoots[0])
	}
	if req.ExecutionRoots[1].Path != canonicalStoredDirectory(additionalRoot) || req.ExecutionRoots[1].Source != executionenv.RootSourceCLIAddDir {
		t.Fatalf("additional binding = %#v", req.ExecutionRoots[1])
	}
}

func TestPrepareRequestExecutionRootsPreservesFrozenCLIOnDrain(t *testing.T) {
	workspaceRoot := t.TempDir()
	additionalRoot := t.TempDir()
	req := api.MessageRequest{ExecutionRoots: []executionenv.RootBinding{{
		Path: additionalRoot, Role: executionenv.RootRoleAdditional,
		AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir, ContextRoot: true,
	}}}
	err := (&RunCoordinator{}).prepareRequestExecutionRoots(context.Background(), &control.Workspace{LocalPath: workspaceRoot}, &req)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.ExecutionRoots) != 2 || req.ExecutionRoots[1].Path != canonicalStoredDirectory(additionalRoot) {
		t.Fatalf("frozen roots were not preserved: %#v", req.ExecutionRoots)
	}
}

func TestRootsExpandWorkspaceAuthorityOnlyForExternalPaths(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "packages", "shared")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	bindings := workspaceRootBindings(&control.Workspace{LocalPath: root})
	bindings = appendRootBinding(bindings, executionenv.RootBinding{
		Path: nested, Source: executionenv.RootSourceCLIAddDir, AccessCap: executionenv.RootAccessWrite,
	})
	if rootsExpandWorkspaceAuthority(bindings) {
		t.Fatal("nested path already covered by the workspace must not downgrade trust")
	}
	bindings = appendRootBinding(bindings, executionenv.RootBinding{
		Path: t.TempDir(), Source: executionenv.RootSourceCLIAddDir, AccessCap: executionenv.RootAccessWrite,
	})
	if !rootsExpandWorkspaceAuthority(bindings) {
		t.Fatal("external path must be treated as an authority expansion")
	}
}

func TestSameCLIAdditionalRootsIgnoresWorkspaceBindings(t *testing.T) {
	a := []executionenv.RootBinding{
		{Path: "/a", Source: executionenv.RootSourceWorkspace},
		{Path: "/shared", Source: executionenv.RootSourceCLIAddDir},
	}
	b := []executionenv.RootBinding{
		{Path: "/b", Source: executionenv.RootSourceWorkspace},
		{Path: "/shared", Source: executionenv.RootSourceCLIAddDir},
	}
	if !sameCLIAdditionalRoots(a, b) {
		t.Fatal("workspace rebinding changed the CLI-root comparison")
	}
}

func TestExternalAddDirDoesNotInheritWorkspaceCapabilities(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	workspace, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "workspace", LocalPath: workspaceRoot, AllowedRoots: []string{workspaceRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantExecutionCapability(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.CapabilityNetworkShared, "network", "human:cli", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: workspace.ID,
		Title: "capability isolation", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := workspaceRootBindings(workspace)
	roots = appendRootBinding(roots, executionenv.RootBinding{
		Path: t.TempDir(), Role: executionenv.RootRoleAdditional,
		AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir, ContextRoot: true,
	})
	run, err := store.StartRunWithOptions(ctx, task, "cli", "inspect", control.StartRunOptions{ExecutionRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &RunCoordinator{srv: &Server{Control: store}}
	lease, err := coordinator.materializeExecutionLease(ctx, identity, run, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.ExecutionCapabilities) != 0 {
		t.Fatalf("external root inherited workspace capabilities: %#v", lease.ExecutionCapabilities)
	}
}
