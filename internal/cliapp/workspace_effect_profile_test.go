package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
)

func TestWorkspaceEffectCommandSendsExactTargetAssertion(t *testing.T) {
	var captured api.WorkspaceEffectProfileRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		"--network", "--workspace", "ws-1", "--", "--region", "east"}); code != 0 {
		t.Fatalf("command failed: code=%d stderr=%q", code, stderr.String())
	}
	if captured.WorkspaceID != "ws-1" || captured.ScriptPath != "scripts/deploy.sh" ||
		len(captured.TargetKeys) != 1 || captured.TargetKeys[0] != "cluster:east" ||
		len(captured.Argv) != 2 || captured.Argv[0] != "--region" || captured.Argv[1] != "east" ||
		!captured.AllowNetwork || !strings.Contains(stdout.String(), "normal execution approval") {
		t.Fatalf("profile request/receipt lost scope: %+v output=%q", captured, stdout.String())
	}
}
