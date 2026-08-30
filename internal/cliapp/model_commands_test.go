package cliapp

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
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

func TestModelManagerOnlyBypassesIncompleteOnboardingReceipt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(), stdout: &stdout, stderr: &stderr,
		configPath: filepath.Join(t.TempDir(), "config.yaml"), modelManagerOnly: true,
	}
	cfg := &config.Config{}

	got, code := app.prepareTUIConfig(cfg)
	if code != 0 || got != cfg {
		t.Fatalf("prepareTUIConfig = cfg:%p code:%d, want original cfg and success", got, code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("model manager entered onboarding: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRestoreStopsOldRuntimeBeforeChangingRoutesAndStartsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetPrimaryModel("codex-cli", "gpt-before", "medium")
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-background"}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	validator := func(_ context.Context, candidate *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
		results := make([]modelchange.ProbeResult, 0, len(routes))
		for _, route := range routes {
			results = append(results, modelchange.ProbeResult{Route: route, OK: true})
		}
		return results
	}
	service := &modelchange.Service{ConfigPath: path, Validate: validator}
	if _, err := service.AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}
	before, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := before.Running
	candidate.Primary.Model = "gpt-candidate"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{
		Candidate: candidate, Source: "test", RequireConfirmation: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.FailStarting(errors.New("restart failed")); err != nil {
		t.Fatal(err)
	}

	var order []string
	app := &App{
		ctx: context.Background(), configPath: path,
		modelRecoveryStop: func() error {
			status, inspectErr := service.Inspect()
			if inspectErr != nil {
				return inspectErr
			}
			if status.Configured != candidate {
				t.Fatalf("configured routes changed before old runtime stopped: %+v", status)
			}
			order = append(order, "stop")
			return nil
		},
		modelRecoveryStart: func() error {
			status, inspectErr := service.Inspect()
			if inspectErr != nil {
				return inspectErr
			}
			if status.Configured != before.Running || status.ModelReady() {
				t.Fatalf("replacement started without restored unverified routes: %+v", status)
			}
			order = append(order, "start")
			return nil
		},
		modelRecoveryWait: func() error {
			order = append(order, "wait")
			return nil
		},
	}
	if _, _, err := app.performModelRecovery(cfg, "restore", prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "stop,start,wait" {
		t.Fatalf("recovery order = %q", got)
	}
}
