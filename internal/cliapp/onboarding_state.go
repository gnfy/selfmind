package cliapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/platform/config"
)

const onboardingStateVersion = 1

type onboardingModelState struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	VerifiedAt time.Time `json:"verified_at,omitempty"`
}

type onboardingState struct {
	Version              int                  `json:"version"`
	Primary              onboardingModelState `json:"primary"`
	Auxiliary            onboardingModelState `json:"auxiliary"`
	AuxiliaryDegraded    bool                 `json:"auxiliary_degraded,omitempty"`
	BackgroundMode       string               `json:"background_mode,omitempty"`
	BackgroundManager    string               `json:"background_manager,omitempty"`
	GatewayVerifiedAt    time.Time            `json:"gateway_verified_at,omitempty"`
	WorkspaceID          string               `json:"workspace_id,omitempty"`
	WorkspaceName        string               `json:"workspace_name,omitempty"`
	WorkspacePath        string               `json:"workspace_path,omitempty"`
	WorkspaceTrusted     bool                 `json:"workspace_trusted,omitempty"`
	ApprovalMode         string               `json:"approval_mode,omitempty"`
	FirstTaskCompleted   bool                 `json:"first_task_completed,omitempty"`
	FirstTaskCompletedAt time.Time            `json:"first_task_completed_at,omitempty"`
	UpdatedAt            time.Time            `json:"updated_at"`
}

func onboardingStatePath(cfg *config.Config, explicitConfigPath string) string {
	path := strings.TrimSpace(explicitConfigPath)
	if cfg != nil && strings.TrimSpace(cfg.Path) != "" {
		path = cfg.Path
	}
	resolved, _ := config.ResolveConfigPath(path)
	return filepath.Join(filepath.Dir(resolved), "onboarding.json")
}

func loadOnboardingState(path string) (onboardingState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return onboardingState{}, nil
	}
	if err != nil {
		return onboardingState{}, err
	}
	var state onboardingState
	if err := json.Unmarshal(data, &state); err != nil {
		return onboardingState{}, fmt.Errorf("decode onboarding state: %w", err)
	}
	return state, nil
}

func saveOnboardingState(path string, state onboardingState) error {
	state.Version = onboardingStateVersion
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".onboarding-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (s onboardingState) matchesModels(cfg *config.Config) bool {
	if cfg == nil || s.Version != onboardingStateVersion {
		return false
	}
	primary := cfg.EffectivePrimary()
	auxiliary := cfg.EffectiveAuxiliary()
	return sameModelRoute(s.Primary, primary) &&
		sameModelRoute(s.Auxiliary, auxiliary) &&
		!s.Primary.VerifiedAt.IsZero() &&
		(!s.Auxiliary.VerifiedAt.IsZero() || s.AuxiliaryDegraded)
}

func sameModelRoute(saved onboardingModelState, current config.ModelSelectionConfig) bool {
	return strings.EqualFold(strings.TrimSpace(saved.Provider), strings.TrimSpace(current.Provider)) &&
		strings.TrimSpace(saved.Model) == strings.TrimSpace(current.Model)
}

func (s onboardingState) coreReady(cfg *config.Config) bool {
	return s.matchesModels(cfg) &&
		(s.BackgroundMode == "managed" || s.BackgroundMode == "on-demand") &&
		!s.GatewayVerifiedAt.IsZero() &&
		strings.TrimSpace(s.WorkspaceID) != "" &&
		strings.TrimSpace(s.WorkspacePath) != "" &&
		s.WorkspaceTrusted &&
		isValidApprovalMode(s.ApprovalMode)
}

func (s *onboardingState) recordModels(cfg *config.Config, auxiliaryDegraded bool) {
	if s == nil || cfg == nil {
		return
	}
	now := time.Now().UTC()
	primary := cfg.EffectivePrimary()
	auxiliary := cfg.EffectiveAuxiliary()
	s.Version = onboardingStateVersion
	s.Primary = onboardingModelState{Provider: primary.Provider, Model: primary.Model, VerifiedAt: now}
	s.Auxiliary = onboardingModelState{Provider: auxiliary.Provider, Model: auxiliary.Model}
	s.AuxiliaryDegraded = auxiliaryDegraded
	if !auxiliaryDegraded {
		s.Auxiliary.VerifiedAt = now
	}
}

func (s *onboardingState) recordFirstTask() {
	if s == nil || s.FirstTaskCompleted {
		return
	}
	s.FirstTaskCompleted = true
	s.FirstTaskCompletedAt = time.Now().UTC()
}
