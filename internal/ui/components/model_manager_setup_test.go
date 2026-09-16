package components

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func setupManagerForTest() *ModelManager {
	m := NewModelManager(ModelManagerStatus{BackgroundEnabled: true}, []ModelManagerProvider{{ID: "one", Models: []ModelManagerModel{{ID: "main"}, {ID: "small"}}}, {ID: "two", CredentialRequired: true, Models: []ModelManagerModel{{ID: "other"}}}}, 80, 24)
	m.SetSetupMode()
	return m
}

func pressSetup(m *ModelManager, keys ...tea.KeyType) ModelManagerAction {
	var action ModelManagerAction
	for _, key := range keys {
		action = m.Update(tea.KeyMsg{Type: key})
	}
	return action
}

func setupProbes() []ModelSetupProbe {
	var probes []ModelSetupProbe
	for _, route := range append([]string{"primary", "auxiliary"}, modelManagerRoles...) {
		probes = append(probes, ModelSetupProbe{Route: route, Provider: "one", Model: "main", OK: true})
	}
	return probes
}

func TestSetupMainBackgroundRolesAndValidationKeyboardFlow(t *testing.T) {
	m := setupManagerForTest()
	if !strings.Contains(m.View(), "Advanced roles") {
		t.Fatal("optional roles are undiscoverable")
	}
	// Main: provider, model; no mandatory reasoning/service-tier pages.
	action := pressSetup(m, tea.KeyEnter, tea.KeyEnter, tea.KeyEnter)
	if action.Closed || action.ValidationRoute != "" || m.screen != modelScreenMenu {
		t.Fatalf("selection should remain an editable draft: %+v", action)
	}
	if !strings.Contains(m.View(), "Same as Main → one/main") {
		t.Fatalf("missing explicit inheritance: %s", m.View())
	}
	// Background uses the current provider's other model, without another key.
	pressSetup(m, tea.KeyEnter, tea.KeyDown, tea.KeyDown, tea.KeyEnter)
	if got := m.setupSelection("background"); got.Model != "small" || got.Reset {
		t.Fatalf("independent Background: %+v", got)
	}
	if strings.Contains(m.View(), "validating…") {
		t.Fatal("editing a setup draft must not claim that a probe has started")
	}
	// Advanced role selects a different provider and its credential once.
	pressSetup(m, tea.KeyUp, tea.KeyEnter, tea.KeyDown, tea.KeyEnter, tea.KeyDown, tea.KeyEnter, tea.KeyDown, tea.KeyEnter)
	if m.screen != modelScreenCredential {
		t.Fatalf("screen=%v, want credential", m.screen)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("demo-key")})
	pressSetup(m, tea.KeyEnter, tea.KeyEnter)
	if got := m.setupSelection("memory_extract"); got.Provider != "two" || got.Model != "other" {
		t.Fatalf("role override: %+v", got)
	}
	pressSetup(m, tea.KeyEsc, tea.KeyDown, tea.KeyEnter)
	if !m.setupValidating {
		t.Fatal("Validate & continue did not request validation")
	}
	if got := pressSetup(m, tea.KeyEnter); got.Closed {
		t.Fatal("repeat Enter applied a pending validation")
	}
	probes := setupProbes()
	probes[3].OK = false
	probes[3].Error = "unavailable"
	m.SetSetupValidation(probes, "", "")
	if !strings.Contains(m.View(), "memory_extract") || m.setupValidated {
		t.Fatal("failed role was hidden or accepted")
	}
	if got := pressSetup(m, tea.KeyEnter); got.Closed || got.ValidationRoute != SetupValidationRoute {
		t.Fatalf("failure must retry, not apply: %+v", got)
	}
	m.SetSetupValidation(setupProbes(), "", "credential-stage")
	apply := pressSetup(m, tea.KeyEnter)
	if !apply.Closed || len(apply.Draft) != 3 {
		t.Fatalf("validated atomic draft: %+v", apply)
	}
	for _, selection := range apply.Draft {
		if selection.APIKey != "" {
			t.Fatal("validated secret was not replaced by stage")
		}
	}
	if m.CredentialStage() != "credential-stage" {
		t.Fatal("credential stage lost")
	}
}

func TestSetupRejectsMissingEvidenceAndEscPreservesDraft(t *testing.T) {
	m := setupManagerForTest()
	pressSetup(m, tea.KeyEnter, tea.KeyEnter, tea.KeyEnter)
	pressSetup(m, tea.KeyEnter, tea.KeyEsc)
	if m.screen != modelScreenMenu || len(m.Draft()) != 2 {
		t.Fatal("Esc should return to summary and preserve draft")
	}
	m.SetSetupValidation(setupProbes()[:1], "", "")
	if m.setupValidated {
		t.Fatal("one successful probe does not verify all roles")
	}
	pressSetup(m, tea.KeyEsc)
	if action := pressSetup(m, tea.KeyEsc); !action.Closed || len(action.Draft) != 0 {
		t.Fatalf("cancel must not submit: %+v", action)
	}
}
