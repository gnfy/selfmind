package components

import (
	"fmt"
	"strings"
)

// ModelSetupProbe is presentation evidence from the daemon, never a local
// readiness authority. Missing route evidence fails closed.
type ModelSetupProbe struct {
	Route, Provider, Model, Error string
	OK                            bool
}

const SetupValidationRoute = "setup"

func (m *ModelManager) setupOptionWindow(headerLines, count int) (int, int) {
	available := m.height - headerLines - 4
	if available < 3 {
		available = 3
	}
	if count <= available {
		return 0, count
	}
	start := m.index - available/2
	if start < 0 {
		start = 0
	}
	if start > count-available {
		start = count - available
	}
	return start, start + available
}

func (m *ModelManager) SetSetupMode() {
	m.setup = true
	if m.status.BackgroundEnabled && m.status.BackgroundProvider == "" && m.status.BackgroundModel == "" {
		m.setDraft(ModelManagerSubmission{Route: "background", Reset: true})
	}
}

func (m *ModelManager) SetSetupValidation(probes []ModelSetupProbe, message, stage string) {
	m.setupValidating = false
	m.setupValidated = message == ""
	m.setupError = message
	m.setupValidation = nil
	for _, route := range append([]string{"primary", "background"}, modelManagerRoles...) {
		result := ModelSetupProbe{Route: route, Error: "validation returned no evidence"}
		for _, probe := range probes {
			if normalizeManagerRoute(probe.Route) == route {
				result = probe
				result.Route = route
				break
			}
		}
		if !result.OK {
			m.setupValidated = false
		}
		m.setupValidation = append(m.setupValidation, result)
	}
	// A credential stage is created only after the whole probe batch passes.
	if m.setupValidated {
		for _, probe := range m.setupValidation {
			m.SetRouteValidation(probe.Route, true, "", stage)
		}
	}
	m.screen, m.index = modelScreenSetupValidation, 0
}

func (m *ModelManager) setupSelection(route string) ModelManagerSubmission {
	selection := m.configuredSelection(route)
	if draft, ok := m.draft[route]; ok {
		selection = draft
	}
	if selection.Reset {
		if route == "background" {
			selection = m.setupSelection("primary")
		} else {
			selection = m.setupSelection("background")
		}
		selection.Route = route
	}
	return selection
}

func (m *ModelManager) setupRouteLabel(route string) string {
	selection := m.setupSelection(route)
	label := strings.Trim(selection.Provider+"/"+selection.Model, "/")
	if label == "" {
		return "not configured"
	}
	return label
}

func (m *ModelManager) setupDraft() []ModelManagerSubmission {
	draft := m.Draft()
	for _, selection := range draft {
		if selection.Route == "primary" {
			return draft
		}
	}
	// The daemon requires an explicit candidate even when repairing only its
	// receipt. Preserve the exact configured Main options in this no-op patch.
	return append([]ModelManagerSubmission{m.configuredSelection("primary")}, draft...)
}

func (m *ModelManager) validateSetup() ModelManagerAction {
	primary := m.setupSelection("primary")
	if primary.Provider == "" || primary.Model == "" {
		m.setupError = "Choose a Main model first."
		return ModelManagerAction{}
	}
	m.setupValidating, m.setupValidated = true, false
	m.setupValidation, m.setupError = nil, ""
	m.screen, m.index = modelScreenSetupValidation, 0
	return ModelManagerAction{ValidationRoute: SetupValidationRoute, Draft: m.setupDraft(), ProviderDraft: m.ProviderDraft()}
}

func (m *ModelManager) finishSetupSelection() ModelManagerAction {
	m.setDraft(m.currentSubmission())
	m.screen, m.index = modelScreenMenu, 3
	if m.route == "primary" {
		m.index = 1
	}
	if isManagerRole(m.route) {
		m.screen, m.index = modelScreenRoles, m.roleIndex
	}
	return ModelManagerAction{}
}

func (m *ModelManager) chooseSetup() (ModelManagerAction, bool) {
	none := ModelManagerAction{}
	switch m.screen {
	case modelScreenMenu:
		switch m.index {
		case 0:
			m.beginRoute("primary")
		case 1:
			m.route = "background"
			m.screen, m.index = modelScreenBackground, 0
		case 2:
			m.screen, m.index = modelScreenRoles, 0
		case 3:
			return m.validateSetup(), true
		case 4:
			return ModelManagerAction{Closed: true}, true
		case 5:
			m.screen, m.index = modelScreenStatus, 0
		}
		return none, true
	case modelScreenModel:
		if m.index < len(m.currentProvider().Models) {
			m.model = m.index
			m.alignTuningOptions()
			return m.finishSetupSelection(), true
		}
	case modelScreenProvider:
		if m.index == len(m.providers) {
			m.screen, m.index = modelScreenConnections, 0
			return none, true
		}
	case modelScreenSetupValidation:
		if m.setupValidated && m.index == 0 {
			return ModelManagerAction{Closed: true, Draft: m.setupDraft(), ProviderDraft: m.ProviderDraft()}, true
		}
		if !m.setupValidated && m.index == 0 {
			return m.validateSetup(), true
		}
		m.screen, m.index = modelScreenMenu, 3
		return none, true
	}
	return none, false
}

func (m *ModelManager) setupOptions() ([]string, bool) {
	switch m.screen {
	case modelScreenMenu:
		roles := "Use Background · optional"
		if count := m.explicitRoleCount(); count > 0 {
			roles = fmt.Sprintf("%d override(s) · optional", count)
		}
		options := []string{"Main             " + m.setupRouteLabel("primary"), "Background       " + m.routeSummary("background"), "Advanced roles   " + roles, "Validate & continue", "Cancel"}
		if m.status.Pending != "" || m.status.ReadinessDegraded {
			options = append(options, "Change status / recovery")
		}
		return options, true
	case modelScreenSetupValidation:
		if m.setupValidating {
			return nil, true
		}
		if m.setupValidated {
			return []string{"Apply models & continue", "Back to models"}, true
		}
		return []string{"Retry validation", "Back to models"}, true
	case modelScreenProvider:
		options := make([]string, 0, len(m.providers)+1)
		for _, provider := range m.providers {
			label := provider.Label
			if label == "" {
				label = provider.ID
			}
			if provider.CredentialReady {
				label += " · credential ready"
			}
			options = append(options, label)
		}
		return append(options, "Provider connections / custom endpoint…"), true
	}
	return nil, false
}

func (m *ModelManager) backSetup() bool {
	switch m.screen {
	case modelScreenBackground:
		m.screen, m.index = modelScreenMenu, 1
	case modelScreenSetupValidation:
		m.screen, m.index = modelScreenMenu, 3
	case modelScreenConnections:
		m.screen, m.index = modelScreenProvider, len(m.providers)
	case modelScreenStatus:
		m.screen, m.index = modelScreenMenu, 0
	case modelScreenProvider:
		if m.route != "background" {
			return false
		}
		m.screen, m.index = modelScreenBackground, 0
	default:
		return false
	}
	return true
}

func (m *ModelManager) setupScreenTitle() (string, bool) {
	switch m.screen {
	case modelScreenMenu:
		return "Which models should SelfMind use?", true
	case modelScreenRoles:
		return "Advanced roles (optional) · default to Background", true
	case modelScreenSetupValidation:
		if m.setupValidating {
			return "Checking model routes…", true
		}
		if m.setupValidated {
			return "All model routes verified", true
		}
		return "Validation needs attention", true
	}
	return "", false
}

func (m *ModelManager) setupDetailLines() []string {
	var lines []string
	if m.screen == modelScreenMenu {
		lines = append(lines, "Background work makes additional model calls.")
	}
	if m.screen == modelScreenSetupValidation {
		for _, probe := range m.setupValidation {
			mark := "✓"
			if !probe.OK {
				mark = "✗"
			}
			line := fmt.Sprintf("%s %s · %s/%s", mark, probe.Route, probe.Provider, probe.Model)
			if probe.Error != "" {
				line += " · " + probe.Error
			}
			lines = append(lines, line)
		}
	}
	if m.setupError != "" {
		lines = append(lines, m.setupError)
	}
	return lines
}
