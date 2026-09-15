package cliapp

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	tui "selfmind/internal/gateway/cli"
	"selfmind/internal/platform/config"
	"selfmind/internal/ui/components"
)

const (
	onboardingCancelled    = -1
	onboardingBackToModels = -2
)

func (a *App) chooseOnboardingRuntime(choice onboardingRuntimeChoice) (onboardingRuntimeChoice, int) {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return choice, 1
	}
	validate := func(path string) (string, error) {
		canonical, err := canonicalOnboardingWorkspace(path)
		if err != nil {
			return "", err
		}
		if onboardingWorkspaceNeedsExplicitChoice(canonical) {
			return "", fmt.Errorf("choose a project directory instead of your home directory or filesystem root")
		}
		return canonical, nil
	}
	model := components.NewRuntimeSetup(choice.WorkspacePath, choice.ApprovalMode, onboardingProtectionSummary(), choice.BackgroundMode == "managed", gatewayServiceSupported(), tui.ResolveTheme(cfg), validate)
	if _, err := tea.NewProgram(model, tea.WithInput(a.stdin), tea.WithOutput(a.stdout), tea.WithContext(a.ctx)).Run(); err != nil {
		fmt.Fprintf(a.stderr, "Runtime setup failed: %v\n", err)
		return choice, 1
	}
	choice.WorkspacePath, choice.ApprovalMode = model.Workspace, model.ApprovalMode
	choice.BackgroundMode = "on-demand"
	if model.Managed {
		choice.BackgroundMode = "managed"
	}
	a.onboardingRuntimeDraft = &choice
	if model.BackToModels {
		return choice, onboardingBackToModels
	}
	if !model.Accepted {
		return choice, onboardingCancelled
	}
	return choice, 0
}
