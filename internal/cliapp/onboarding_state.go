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

const onboardingStateVersion = 2

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
	ServiceGeneration    string               `json:"service_generation,omitempty"`
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

type onboardingStateV2 struct {
	Version              int       `json:"version"`
	BackgroundMode       string    `json:"background_mode,omitempty"`
	BackgroundManager    string    `json:"background_manager,omitempty"`
	ServiceGeneration    string    `json:"service_generation,omitempty"`
	GatewayVerifiedAt    time.Time `json:"gateway_verified_at,omitempty"`
	WorkspaceID          string    `json:"workspace_id,omitempty"`
	WorkspaceName        string    `json:"workspace_name,omitempty"`
	WorkspacePath        string    `json:"workspace_path,omitempty"`
	WorkspaceTrusted     bool      `json:"workspace_trusted,omitempty"`
	ApprovalMode         string    `json:"approval_mode,omitempty"`
	FirstTaskCompleted   bool      `json:"first_task_completed,omitempty"`
	FirstTaskCompletedAt time.Time `json:"first_task_completed_at,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
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
	legacyVersion := state.Version
	state.Version = onboardingStateVersion
	state.UpdatedAt = time.Now().UTC()
	wire := onboardingStateV2{
		Version: state.Version, BackgroundMode: state.BackgroundMode, BackgroundManager: state.BackgroundManager,
		ServiceGeneration: state.ServiceGeneration,
		GatewayVerifiedAt: state.GatewayVerifiedAt, WorkspaceID: state.WorkspaceID, WorkspaceName: state.WorkspaceName,
		WorkspacePath: state.WorkspacePath, WorkspaceTrusted: state.WorkspaceTrusted, ApprovalMode: state.ApprovalMode,
		FirstTaskCompleted: state.FirstTaskCompleted, FirstTaskCompletedAt: state.FirstTaskCompletedAt, UpdatedAt: state.UpdatedAt,
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if legacyVersion > 0 && legacyVersion < onboardingStateVersion {
		if err := backupOnboardingState(path, legacyVersion); err != nil {
			return err
		}
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

func backupOnboardingState(path string, version int) error {
	backup := fmt.Sprintf("%s.v%d.backup", path, version)
	if _, err := os.Stat(backup); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeOnboardingStateAtomic(backup, data)
}

func writeOnboardingStateAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".onboarding-backup-*.tmp")
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
	if cfg == nil || s.Version != 1 || s.AuxiliaryDegraded {
		return false
	}
	primary := cfg.EffectivePrimary()
	auxiliary := cfg.EffectiveAuxiliary()
	return sameModelRoute(s.Primary, primary) &&
		sameModelRoute(s.Auxiliary, auxiliary) &&
		!s.Primary.VerifiedAt.IsZero() &&
		!s.Auxiliary.VerifiedAt.IsZero()
}

func sameModelRoute(saved onboardingModelState, current config.ModelSelectionConfig) bool {
	return strings.EqualFold(strings.TrimSpace(saved.Provider), strings.TrimSpace(current.Provider)) &&
		strings.TrimSpace(saved.Model) == strings.TrimSpace(current.Model)
}

func (s onboardingState) runtimeReady() bool {
	return (s.BackgroundMode == "managed" || s.BackgroundMode == "on-demand") &&
		!s.GatewayVerifiedAt.IsZero() &&
		strings.TrimSpace(s.WorkspaceID) != "" &&
		strings.TrimSpace(s.WorkspacePath) != "" &&
		s.WorkspaceTrusted &&
		isValidApprovalMode(s.ApprovalMode)
}

func (s *onboardingState) retireLegacyModels() {
	if s == nil {
		return
	}
	s.Version = onboardingStateVersion
	s.Primary = onboardingModelState{}
	s.Auxiliary = onboardingModelState{}
	s.AuxiliaryDegraded = false
}

func (s *onboardingState) recordFirstTask() {
	if s == nil || s.FirstTaskCompleted {
		return
	}
	s.FirstTaskCompleted = true
	s.FirstTaskCompletedAt = time.Now().UTC()
}
