package cliapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/platform/config"
)

func TestModelSetWritesExplicitConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model", "set", "openai", "gpt-test"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runModelCommandIfRequested()
	if !handled {
		t.Fatal("model command was not handled")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.EffectiveProvider(), "openai"; got != want {
		t.Fatalf("provider = %q, want %q", got, want)
	}
	if got, want := cfg.EffectiveModel(), "gpt-test"; got != want {
		t.Fatalf("model = %q, want %q", got, want)
	}
	if cfg.Models.Primary.Provider != "openai" || cfg.Models.Primary.Model != "gpt-test" {
		t.Fatalf("primary = %+v", cfg.Models.Primary)
	}
	if cfg.Model.Provider != "" || cfg.Agent.Provider != "" {
		t.Fatalf("legacy selection was persisted: model=%+v agent=%+v", cfg.Model, cfg.Agent)
	}
}

func TestModelSetValidatesDynamicCodexReasoning(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{
  "models": [{
    "slug": "gpt-5.6-sol",
    "context_window": 272000,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": [{"effort":"low"},{"effort":"xhigh"}]
  }]
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model", "set", "codex-cli", "gpt-5.6-sol", "--reasoning", "xhigh"},
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		configPath: path,
	}
	_, code := app.runModelCommandIfRequested()
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, app.stderr)
	}
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectivePrimary(); got.Reasoning != "xhigh" || got.Model != "gpt-5.6-sol" {
		t.Fatalf("primary = %+v", got)
	}

	app.args = []string{"selfmind", "model", "set", "codex-cli", "gpt-5.6-sol", "--reasoning", "unsupported"}
	if _, code := app.runModelCommandIfRequested(); code != 2 {
		t.Fatalf("unsupported reasoning code=%d, want 2", code)
	}
}

func TestModelCheckResolvesConfiguredProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveConfig(path, &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {APIKey: "sk-kimi-test"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model", "check"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runModelCommandIfRequested()
	if !handled {
		t.Fatal("model command was not handled")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resolved: provider=kimi-coding model=kimi-for-coding protocol=anthropic_messages") {
		t.Fatalf("stdout missing resolved provider: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "sk-kimi-test") {
		t.Fatalf("stdout leaked API key: %s", stdout.String())
	}
}

func TestModelCheckLiveRoleValidatesNativeToolSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []map[string]interface{} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		function := request.Tools[0]["function"].(map[string]interface{})
		parameters := function["parameters"].(map[string]interface{})
		if required, exists := parameters["required"]; exists {
			t.Fatalf("required must be omitted, got %#v", required)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveConfig(path, &config.Config{
		Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
			"fast_classifier": {
				Provider: "deepseek",
				Model:    "deepseek-v4-flash",
				BaseURL:  server.URL,
				Protocol: "openai_compatible",
				APIKey:   "test-key",
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model", "check", "--live", "--role", "fast_classifier"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}
	handled, code := app.runModelCommandIfRequested()
	if !handled || code != 0 {
		t.Fatalf("handled=%t code=%d stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "native tool schema: passed") {
		t.Fatalf("stdout missing schema result: %s", stdout.String())
	}
}

func TestModelCheckReportsTokenRefresher(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	token := cliappFakeJWT(time.Now().Add(time.Hour))
	authJSON := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"refresh-token"}}`, token)
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(authJSON), 0600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveConfig(path, &config.Config{
		Model: config.ModelConfig{Provider: "codex-cli", Default: "gpt-5.5"},
	}); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model", "check"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runModelCommandIfRequested()
	if !handled {
		t.Fatal("model command was not handled")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Token getter: configured") {
		t.Fatalf("stdout missing token getter: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Token refresher: configured") {
		t.Fatalf("stdout missing token refresher: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stdout.String(), "refresh-token") {
		t.Fatalf("stdout leaked token material: %s", stdout.String())
	}
}

func TestModelCheckReportsMissingCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveConfig(path, &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
	}); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model", "check"},
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runModelCommandIfRequested()
	if !handled {
		t.Fatal("model command was not handled")
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no credentials found for provider kimi-coding") {
		t.Fatalf("stderr missing credential error: %s", stderr.String())
	}
}

func cliappFakeJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + claims + ".sig"
}

func TestInteractiveModelCommandAddsCustomEndpointFromPipedInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	input := strings.Join([]string{
		"4",
		server.URL + "/v1",
		"",
		"",
		"",
		"",
	}, "\n") + "\n"
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "model"},
		stdin:      strings.NewReader(input),
		stdout:     stdout,
		stderr:     stderr,
		configPath: path,
	}

	handled, code := app.runModelCommandIfRequested()
	if !handled {
		t.Fatal("model command was not handled")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}

	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.EffectiveProvider(), "custom:127"; got != want {
		t.Fatalf("provider = %q, want %q", got, want)
	}
	if got, want := cfg.EffectiveModel(), "local-model"; got != want {
		t.Fatalf("model = %q, want %q", got, want)
	}
	if len(cfg.Providers.Custom) != 1 {
		t.Fatalf("custom providers = %d, want 1", len(cfg.Providers.Custom))
	}
	if got, want := cfg.Providers.Custom[0].BaseURL, server.URL+"/v1"; got != want {
		t.Fatalf("base url = %q, want %q", got, want)
	}
}
