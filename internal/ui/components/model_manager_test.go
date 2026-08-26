package components

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestModelManagerExposesMainBackgroundRolesStatusAndExit(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, nil, 80, 24)
	view := manager.View()
	for _, want := range []string{"Main model", "Background model", "Role overrides", "Change status", "Exit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %s", want, view)
		}
	}
}

func TestModelManagerBuildsOneDraftAcrossMainBackgroundAndRole(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{
		ID: "codex-cli", Label: "Codex", Models: []ModelManagerModel{{ID: "gpt-next"}},
	}}, 80, 24)

	// Main model: provider -> model -> reasoning -> service tier.
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for i := 0; i < 3; i++ {
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.ValidationRoute != "primary" || len(action.Draft) != 1 {
		t.Fatalf("main validation action = %+v", action)
	}

	// Background model.
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for i := 0; i < 4; i++ {
		action = manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if action.ValidationRoute != "background" || len(action.Draft) != 2 {
		t.Fatalf("background validation action = %+v", action)
	}

	// Role overrides -> memory_extract -> choose explicit -> route wizard.
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for i := 0; i < 4; i++ {
		action = manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if action.ValidationRoute != "memory_extract" || len(action.Draft) != 3 {
		t.Fatalf("role validation action = %+v", action)
	}

	// Review and apply returns the entire draft once.
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	apply := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !apply.Closed || len(apply.Draft) != 3 {
		t.Fatalf("apply action = %+v", apply)
	}
}

func TestModelManagerRecoveryOffersRetryAndRestore(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{RecoveryRequired: true, RecoveryFailure: "port unavailable"}, nil, 80, 24)
	if view := manager.View(); !strings.Contains(view, "Gateway recovery required") || !strings.Contains(view, "port unavailable") {
		t.Fatalf("view = %q", view)
	}
	if action := manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); !action.Closed || action.RecoveryAction != "retry" {
		t.Fatalf("retry action = %+v", action)
	}
	manager = NewModelManager(ModelManagerStatus{RecoveryRequired: true}, nil, 80, 24)
	if action := manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")}); !action.Closed || action.RecoveryAction != "restore" {
		t.Fatalf("restore action = %+v", action)
	}
}

func TestModelManagerPreservesConfiguredRouteOptions(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{
		PrimaryProvider: "codex-cli", PrimaryModel: "gpt-next", PrimaryReasoning: "high", PrimaryServiceTier: "priority",
	}, []ModelManagerProvider{{
		ID: "codex-cli", Models: []ModelManagerModel{{ID: "gpt-next", Reasoning: []string{"low", "high"}, ServiceTiers: []string{"priority"}}},
	}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // main
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // model
	if manager.option(manager.reasoningOptions(), manager.index) != "high" {
		t.Fatalf("reasoning selection = %q", manager.option(manager.reasoningOptions(), manager.index))
	}
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // reasoning
	if manager.option(manager.serviceTierOptions(), manager.index) != "priority" {
		t.Fatalf("service tier selection = %q", manager.option(manager.serviceTierOptions(), manager.index))
	}
}

func TestModelManagerAcceptsManualModelID(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{ID: "custom:test"}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // route
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // manual model entry
	manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("future-model")})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})           // accept id
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})           // reasoning
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // service tier
	if len(action.Draft) != 1 || action.Draft[0].Model != "future-model" {
		t.Fatalf("action = %+v", action)
	}
}

func TestModelManagerCollectsMissingProviderCredentialBeforeValidation(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{
		ID: "deepseek", CredentialRequired: true, Models: []ModelManagerModel{{ID: "deepseek-chat"}},
	}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // main
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider
	if view := manager.View(); !strings.Contains(view, "API key") {
		t.Fatalf("view = %q", view)
	}
	manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("sk-secret")})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})           // credential
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})           // model
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})           // reasoning
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // service tier
	if len(action.Draft) != 1 || action.Draft[0].APIKey != "sk-secret" {
		t.Fatalf("action = %+v", action)
	}
	if strings.Contains(manager.View(), "sk-secret") {
		t.Fatal("credential leaked into the rendered view")
	}
	manager.SetRouteValidation("primary", false, "authentication failed")
	if got := manager.Draft()[0].APIKey; got != "sk-secret" {
		t.Fatalf("failed validation discarded retry credential: %q", got)
	}
	manager.SetRouteValidation("primary", true, "")
	if got := manager.Draft()[0].APIKey; got != "" {
		t.Fatalf("validated draft retained credential: %q", got)
	}
	if !manager.providers[0].CredentialReady {
		t.Fatal("validated provider was not marked credential-ready")
	}
}

func TestModelManagerEscapeClosesWithoutSubmission(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, nil, 80, 24)
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !action.Closed || action.Submission != nil {
		t.Fatalf("action = %+v", action)
	}
}
