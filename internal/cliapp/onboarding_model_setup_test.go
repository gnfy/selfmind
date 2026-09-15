package cliapp

import (
	"bytes"
	"path/filepath"
	"testing"

	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
)

func TestOnboardingReloadsAppliedModelConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, configPath: path}
	app.onboardingModelSetup = func(*config.Config) (bool, error) {
		updated, err := config.LoadConfig(config.Options{Path: path})
		if err != nil {
			return false, err
		}
		updated.Models.Primary = config.ModelSelectionConfig{Provider: "test", Model: "new"}
		updated.Models.Auxiliary = config.ModelSelectionConfig{FollowPrimary: true}
		if err := config.SaveConfig(path, updated); err != nil {
			return false, err
		}
		_, err = (&modelchange.Service{ConfigPath: path}).AcceptMigrationReadiness()
		return err == nil, err
	}
	got, code := app.finishOnboardingModels(cfg)
	if code != 0 || got == nil || got.EffectivePrimary().Model != "new" || !got.Models.Auxiliary.FollowPrimary {
		t.Fatalf("setup resumed a stale configuration: cfg=%+v code=%d", got, code)
	}
}

func TestOnboardingRejectsUnappliedModelManagerCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, onboardingModelSetup: func(*config.Config) (bool, error) { return true, nil }}
	if got, code := app.finishOnboardingModels(cfg); code == 0 || got != nil {
		t.Fatal("UI completion alone established Model Readiness")
	}
}
