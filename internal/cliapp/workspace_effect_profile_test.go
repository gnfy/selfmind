package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
)

func TestWorkspaceEffectCommandSendsExactTargetAssertion(t *testing.T) {
	var captured api.WorkspaceEffectProfileRequest
	server := newFakeGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/effect-profiles" {
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode profile: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"workspace_id": "ws-1",
			"profile": map[string]string{"label": "reviewed effect"}})
	}))
	defer server.Close()
	t.Setenv("SELF_GATEWAY_URL", server.URL)
	var stdout, stderr bytes.Buffer
	a := &App{ctx: context.Background(), stdout: &stdout, stderr: &stderr, gatewayEnsured: true}
	if code := a.registerWorkspaceEffectProfile([]string{"scripts/deploy.sh", "--target", "cluster:east",
		"--network", "--observe-command", "./scripts/state.sh east", "--workspace", "ws-1", "--", "--region", "east"}); code != 0 {
		t.Fatalf("command failed: code=%d stderr=%q", code, stderr.String())
	}
	if captured.WorkspaceID != "ws-1" || captured.ScriptPath != "scripts/deploy.sh" ||
		len(captured.TargetKeys) != 1 || captured.TargetKeys[0] != "cluster:east" ||
		len(captured.Argv) != 2 || captured.Argv[0] != "--region" || captured.Argv[1] != "east" ||
		captured.ObservationCommand != "./scripts/state.sh east" ||
		!captured.AllowNetwork || !strings.Contains(stdout.String(), "normal execution approval") {
		t.Fatalf("profile request/receipt lost scope: %+v output=%q", captured, stdout.String())
	}
}

// The rest of the exact invocation travels with the profile: the directory it
// runs from, its declared environment, and its one standard input.
func TestWorkspaceEffectCommandSendsDeclaredInvocationInputs(t *testing.T) {
	var captured api.WorkspaceEffectProfileRequest
	server := newFakeGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode profile: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"workspace_id": "ws-1",
			"profile": map[string]string{"label": "reviewed effect"}})
	}))
	defer server.Close()
	t.Setenv("SELF_GATEWAY_URL", server.URL)
	var stdout, stderr bytes.Buffer
	a := &App{ctx: context.Background(), stdout: &stdout, stderr: &stderr, gatewayEnsured: true}
	if code := a.registerWorkspaceEffectProfile([]string{"scripts/deploy.sh", "--target", "cluster:west",
		"--cwd", "scripts", "--env", "DEPLOY_REGION=eu-1", "--env", "DRY_RUN=0", "--stdin-file", "config/payload.json",
		"--", "west"}); code != 0 {
		t.Fatalf("command failed: code=%d stderr=%q", code, stderr.String())
	}
	if captured.Cwd != "scripts" || len(captured.Env) != 2 || captured.Env[0] != "DEPLOY_REGION=eu-1" ||
		captured.Env[1] != "DRY_RUN=0" || captured.StdinFile != "config/payload.json" || captured.StdinText != nil ||
		len(captured.Argv) != 1 || captured.Argv[0] != "west" {
		t.Fatalf("declared inputs were not sent: %+v", captured)
	}

	captured = api.WorkspaceEffectProfileRequest{}
	if code := a.registerWorkspaceEffectProfile([]string{"scripts/deploy.sh", "--target", "cluster:east",
		"--stdin", ""}); code != 0 || captured.StdinText == nil || *captured.StdinText != "" {
		t.Fatalf("empty here-string text was not declared: code=%d request=%+v stderr=%q", code, captured, stderr.String())
	}

	stderr.Reset()
	if code := a.registerWorkspaceEffectProfile([]string{"scripts/deploy.sh", "--target", "cluster:east",
		"--stdin", "yes", "--stdin-file", "config/payload.json"}); code != 2 || !strings.Contains(stderr.String(), "one standard input") {
		t.Fatalf("two standard inputs were accepted: code=%d stderr=%q", code, stderr.String())
	}
}
