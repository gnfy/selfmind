package cliapp

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactHeaderValueMasksSecretsAndAccountIdentity(t *testing.T) {
	for _, key := range []string{"Authorization", "X-API-Key", "chatgpt-account-id", "X-User-ID"} {
		if got := redactHeaderValue(key, "sensitive-value"); got != "***" {
			t.Fatalf("redactHeaderValue(%q) = %q, want masked", key, got)
		}
	}
	if got := redactHeaderValue("User-Agent", "selfmind-test"); got != "selfmind-test" {
		t.Fatalf("User-Agent = %q, want visible compatibility value", got)
	}
}

func TestModelRejectsEveryLegacySubcommand(t *testing.T) {
	for _, legacy := range []string{
		"primary", "background", "auxiliary", "current", "history", "confirm", "cancel",
		"rollback", "recover", "check", "list", "set",
	} {
		t.Run(legacy, func(t *testing.T) {
			stderr := &bytes.Buffer{}
			app := &App{
				ctx:        context.Background(),
				args:       []string{"selfmind", "model", legacy},
				stdout:     &bytes.Buffer{},
				stderr:     stderr,
				configPath: filepath.Join(t.TempDir(), "config.yaml"),
			}

			handled, code := app.runModelCommandIfRequested()
			if !handled {
				t.Fatal("model command was not handled")
			}
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if got := strings.TrimSpace(stderr.String()); got != "Usage: selfmind model" {
				t.Fatalf("stderr = %q, want the single public command", got)
			}
		})
	}
}
