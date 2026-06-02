package cliapp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
