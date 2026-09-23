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
	manager.Update(tea.KeyMsg{Type: tea.KeyDown}) // explicit gpt-next
	for i := 0; i < 3; i++ {
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
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	apply := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !apply.Closed || len(apply.Draft) != 3 {
		t.Fatalf("apply action = %+v", apply)
	}
}

// The daemon runs approval triage at the chosen model's lowest-latency tier
// whatever reasoning is configured, so the manager must not offer a reasoning
// choice there that would be ignored. Other roles keep theirs.
func TestModelManagerApprovalRoleOffersNoReasoningChoice(t *testing.T) {
	pickExplicitModel := func(roleDowns int) *ModelManager {
		manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{
			ID: "codex-cli", Label: "Codex", Models: []ModelManagerModel{{ID: "gpt-next", Reasoning: []string{"low", "high"}}},
		}}, 80, 24)
		// Role overrides -> role -> choose explicit -> provider -> model.
		manager.Update(tea.KeyMsg{Type: tea.KeyDown})
		manager.Update(tea.KeyMsg{Type: tea.KeyDown})
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		for i := 0; i < roleDowns; i++ {
			manager.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		manager.Update(tea.KeyMsg{Type: tea.KeyDown})
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return manager
	}

	approval := pickExplicitModel(0)
	if approval.route != "fast_classifier" || approval.screen != modelScreenServiceTier {
		t.Fatalf("approval route %q is on screen %v after choosing the model, want the service tier", approval.route, approval.screen)
	}
	approval.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if approval.screen != modelScreenModel {
		t.Fatalf("back from the approval service tier landed on screen %v, want the model list", approval.screen)
	}
	approval.Update(tea.KeyMsg{Type: tea.KeyEnter})
	action := approval.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.ValidationRoute != "fast_classifier" || len(action.Draft) != 1 || action.Draft[0].Model != "gpt-next" {
		t.Fatalf("approval draft action = %+v", action)
	}
	if got := action.Draft[0].Reasoning; got != "auto" {
		t.Fatalf("approval draft reasoning = %q, want auto", got)
	}

	if review := pickExplicitModel(1); review.route != "memory_extract" || review.screen != modelScreenReasoning {
		t.Fatalf("memory_extract route %q is on screen %v after choosing the model, want its reasoning choice", review.route, review.screen)
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

func TestModelManagerAcceptsManualReasoningWhenCapabilitiesAreUnknown(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{
		ID: "custom:test", Models: []ModelManagerModel{{ID: "future-model"}},
	}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // main
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // model
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})  // manual reasoning
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !manager.editingCustomReasoning {
		t.Fatal("manual reasoning editor did not open")
	}
	manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ultra")})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // reasoning -> service tier
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(action.Draft) != 1 || action.Draft[0].Reasoning != "ultra" {
		t.Fatalf("action = %+v", action)
	}
}

func TestModelManagerShowsAndForgetsRememberedModel(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{
		ID: "google", Models: []ModelManagerModel{{ID: "gemini-retired", Remembered: true}},
	}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // main -> provider
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider -> model
	view := manager.View()
	if !strings.Contains(view, "gemini-retired · remembered") || !strings.Contains(view, "d forget remembered") {
		t.Fatalf("remembered model affordance is missing: %s", view)
	}
	action := manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if action.ForgetModel == nil || action.ForgetModel.Provider != "google" || action.ForgetModel.Model != "gemini-retired" {
		t.Fatalf("forget action = %+v", action)
	}
	manager.ForgetRememberedModel("google", "gemini-retired")
	if got := manager.currentProvider().Models; len(got) != 0 {
		t.Fatalf("history-only model remained after forgetting: %+v", got)
	}
}

func TestModelManagerForgetKeepsCatalogOrConfiguredModel(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{
		ID: "google", Models: []ModelManagerModel{
			{ID: "catalog-model", Remembered: true, Available: true},
			{ID: "configured-model", Remembered: true, Configured: true},
		},
	}}, 80, 24)
	manager.ForgetRememberedModel("google", "catalog-model")
	manager.ForgetRememberedModel("google", "configured-model")
	models := manager.providers[0].Models
	if len(models) != 2 || models[0].Remembered || models[1].Remembered {
		t.Fatalf("forget removed provider/configured availability: %+v", models)
	}
}

func TestModelManagerPreservesExplicitReasoningWhenCapabilitiesAreUnknown(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{
		PrimaryProvider: "custom:test", PrimaryModel: "future-model", PrimaryReasoning: "ultra",
	}, []ModelManagerProvider{{
		ID: "custom:test", Models: []ModelManagerModel{{ID: "future-model"}},
	}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // main
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // model
	if got := manager.option(manager.reasoningOptions(), manager.index); got != "ultra" {
		t.Fatalf("reasoning selection = %q", got)
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
	manager.SetRouteValidation("primary", false, "authentication failed", "")
	if got := manager.Draft()[0].APIKey; got != "sk-secret" {
		t.Fatalf("failed validation discarded retry credential: %q", got)
	}
	manager.SetRouteValidation("primary", true, "", "stage-one")
	if got := manager.Draft()[0].APIKey; got != "" {
		t.Fatalf("validated draft retained credential: %q", got)
	}
	if !manager.providers[0].CredentialReady {
		t.Fatal("validated provider was not marked credential-ready")
	}
	if manager.CredentialStage() != "stage-one" {
		t.Fatalf("credential stage = %q", manager.CredentialStage())
	}
}

func TestModelManagerEscapeClosesWithoutSubmission(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, nil, 80, 24)
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !action.Closed || action.Submission != nil {
		t.Fatalf("action = %+v", action)
	}
}

func TestModelManagerCanDisableBackgroundWork(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{
		PrimaryProvider: "openai", PrimaryModel: "gpt-test",
		BackgroundEnabled: true, BackgroundProvider: "openai", BackgroundModel: "gpt-test",
	}, []ModelManagerProvider{{ID: "openai", Models: []ModelManagerModel{{ID: "gpt-test"}}}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})  // background menu
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // background choices
	for range 3 {
		manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.ValidationRoute != "background" || len(action.Draft) != 1 || action.Draft[0].Reset || action.Draft[0].Enabled == nil || *action.Draft[0].Enabled {
		t.Fatalf("disable action = %+v", action)
	}
	if summary := manager.routeSummary("background"); !strings.Contains(summary, "disabled") {
		t.Fatalf("background summary = %q", summary)
	}
}

func TestModelManagerBackgroundChoicesMatchSetupAndLaterSettings(t *testing.T) {
	status := ModelManagerStatus{
		PrimaryProvider: "one", PrimaryModel: "main",
		BackgroundEnabled: true, BackgroundProvider: "one", BackgroundModel: "small",
	}
	providers := []ModelManagerProvider{
		{ID: "one", Models: []ModelManagerModel{{ID: "main"}, {ID: "small"}}},
		{ID: "two", Models: []ModelManagerModel{{ID: "other"}}},
	}
	normal := NewModelManager(status, providers, 80, 24)
	normal.Update(tea.KeyMsg{Type: tea.KeyDown})
	normal.Update(tea.KeyMsg{Type: tea.KeyEnter})

	setup := NewModelManager(status, providers, 80, 24)
	setup.SetSetupMode()
	setup.Update(tea.KeyMsg{Type: tea.KeyDown})
	setup.Update(tea.KeyMsg{Type: tea.KeyEnter})

	want := []string{"Same as Main", "main", "small", "Another Provider…", "Disable background model work", "Back"}
	if got := normal.options(); !equalStrings(got, want) {
		t.Fatalf("normal background choices = %v, want %v", got, want)
	}
	if got := setup.options(); !equalStrings(got, want) {
		t.Fatalf("setup background choices = %v, want %v", got, want)
	}
	for _, manager := range []*ModelManager{normal, setup} {
		view := manager.View()
		for _, text := range []string{"What should Background use?", "Main: one/main", "Same as Main follows future Main changes; a named model stays independent."} {
			if !strings.Contains(view, text) {
				t.Fatalf("shared Background view missing %q: %s", text, view)
			}
		}
	}
}

func TestModelManagerCanRestoreBackgroundInheritance(t *testing.T) {
	for _, status := range []ModelManagerStatus{
		{
			PrimaryProvider: "one", PrimaryModel: "main",
			BackgroundEnabled: true, BackgroundProvider: "one", BackgroundModel: "small",
		},
		{
			PrimaryProvider: "one", PrimaryModel: "main",
			BackgroundEnabled: false, BackgroundProvider: "one", BackgroundModel: "small",
		},
	} {
		manager := NewModelManager(status, []ModelManagerProvider{{ID: "one", Models: []ModelManagerModel{{ID: "main"}, {ID: "small"}}}}, 80, 24)
		manager.Update(tea.KeyMsg{Type: tea.KeyDown})
		manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if action.ValidationRoute != "background" || len(action.Draft) != 1 || !action.Draft[0].Reset || action.Draft[0].Enabled != nil {
			t.Fatalf("inheritance action = %+v", action)
		}
		if summary := manager.routeSummary("background"); summary != "Same as Main → one/main" {
			t.Fatalf("background summary = %q", summary)
		}
		manager.screen = modelScreenReview
		if view := manager.View(); !strings.Contains(view, "Same as Main") {
			t.Fatalf("review hides inheritance: %s", view)
		}
	}
}

func TestModelManagerCurrentBackgroundChoiceIsANoOp(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{
		PrimaryProvider: "one", PrimaryModel: "main",
		BackgroundEnabled: true, BackgroundFollowsMain: true,
	}, []ModelManagerProvider{{ID: "one", Models: []ModelManagerModel{{ID: "main"}}}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.ValidationRoute != "" || len(action.Draft) != 0 || manager.screen != modelScreenMenu {
		t.Fatalf("current inheritance should not start an empty validation: %+v", action)
	}
}

func TestModelManagerExplicitMainModelKeepsBackgroundIndependent(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{
		PrimaryProvider: "one", PrimaryModel: "main",
		BackgroundEnabled: true, BackgroundFollowsMain: true,
	}, []ModelManagerProvider{{ID: "one", Models: []ModelManagerModel{{ID: "main"}}}}, 80, 24)
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // explicit main model
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // reasoning
	action := manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.ValidationRoute != "background" || len(action.Draft) != 1 {
		t.Fatalf("explicit Background action = %+v", action)
	}
	selection := action.Draft[0]
	if selection.Reset || selection.Provider != "one" || selection.Model != "main" || selection.Enabled == nil || !*selection.Enabled {
		t.Fatalf("explicit Background selection = %+v", selection)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestModelManagerAddsCustomProviderConnectionDraft(t *testing.T) {
	manager := NewModelManager(ModelManagerStatus{}, []ModelManagerProvider{{ID: "openai"}}, 100, 30)
	for range 3 {
		manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // provider connections
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})  // add custom
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Company_Gateway")})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	manager.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("https://llm.company.example/v1/")})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // openai-compatible
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyDown})
	manager.Update(tea.KeyMsg{Type: tea.KeyEnter}) // auth none
	draft := manager.ProviderDraft()
	if len(draft) != 1 || draft[0].ID != "company-gateway" || draft[0].BaseURL != "https://llm.company.example/v1" || draft[0].Protocol != "openai-compatible" || draft[0].Auth != "none" {
		t.Fatalf("provider draft = %+v", draft)
	}
	if !manager.providerIDExists("company-gateway") {
		t.Fatal("new connection was not available to route selection")
	}
}
