package components

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	uitheme "selfmind/internal/ui/theme"
)

var modelManagerRoles = []string{
	"fast_classifier",
	"memory_extract",
	"background_review",
	"skill_curator",
	"semantic_recall",
	"summarizer",
}

type ModelManagerStatus struct {
	RunningPrimary        string
	RunningBackground     string
	ConfiguredPrimary     string
	ConfiguredBackground  string
	PrimaryProvider       string
	PrimaryModel          string
	PrimaryReasoning      string
	PrimaryServiceTier    string
	BackgroundProvider    string
	BackgroundModel       string
	BackgroundReasoning   string
	BackgroundServiceTier string
	BackgroundEnabled     bool
	ForegroundReady       bool
	BackgroundReady       bool
	ReadinessDegraded     bool
	ForegroundReason      string
	BackgroundReason      string
	RoleOverrides         map[string]ModelManagerSubmission
	Pending               string
	RecoveryRequired      bool
	RecoveryFailure       string
	Generation            int64
}

type ModelManagerModel struct {
	ID           string
	Reasoning    []string
	ServiceTiers []string
}

type ModelManagerProvider struct {
	ID                 string
	Label              string
	Source             string
	CredentialRequired bool
	CredentialReady    bool
	Models             []ModelManagerModel
	Custom             bool
	BaseURL            string
	Protocol           string
	Auth               string
}

type ModelManagerProviderSubmission struct {
	ID       string
	Custom   bool
	BaseURL  string
	Protocol string
	Auth     string
	Delete   bool
}

type ModelManagerSubmission struct {
	Route       string
	Provider    string
	Model       string
	Reasoning   string
	ServiceTier string
	Reset       bool
	APIKey      string
	Enabled     *bool
}

type ModelManagerAction struct {
	Closed          bool
	Submission      *ModelManagerSubmission // compatibility for older embedders
	Draft           []ModelManagerSubmission
	ValidationRoute string
	RecoveryAction  string
	ProviderDraft   []ModelManagerProviderSubmission
}

type modelManagerScreen int

const (
	modelScreenMenu modelManagerScreen = iota
	modelScreenRoles
	modelScreenRoleChoice
	modelScreenProvider
	modelScreenCredential
	modelScreenModel
	modelScreenReasoning
	modelScreenServiceTier
	modelScreenReview
	modelScreenStatus
	modelScreenConnections
	modelScreenConnectionDetail
	modelScreenConnectionName
	modelScreenConnectionURL
	modelScreenConnectionProtocol
	modelScreenConnectionAuth
)

// ModelManager owns one transient, multi-route draft. The daemon remains the
// authority for probes, persistence, generation checks, and safe restarts.
type ModelManager struct {
	theme              uitheme.Theme
	status             ModelManagerStatus
	providers          []ModelManagerProvider
	screen             modelManagerScreen
	index              int
	route              string
	roleIndex          int
	provider           int
	model              int
	reasoning          int
	serviceTier        int
	draft              map[string]ModelManagerSubmission
	providerDraft      map[string]ModelManagerProviderSubmission
	validation         map[string]string
	credentials        map[string]string
	credentialInput    []rune
	credentialStage    string
	editingCustomModel bool
	customModelInput   []rune
	connection         int
	connectionNew      bool
	connectionInput    []rune
	connectionEditing  ModelManagerProviderSubmission
	width              int
	height             int
}

func NewModelManager(status ModelManagerStatus, providers []ModelManagerProvider, width, height int) *ModelManager {
	return NewModelManagerWithTheme(status, providers, width, height, uitheme.Default())
}

func NewModelManagerWithTheme(status ModelManagerStatus, providers []ModelManagerProvider, width, height int, t uitheme.Theme) *ModelManager {
	m := &ModelManager{
		theme:  t,
		status: status, providers: append([]ModelManagerProvider(nil), providers...),
		draft: make(map[string]ModelManagerSubmission), providerDraft: make(map[string]ModelManagerProviderSubmission),
		validation: make(map[string]string), credentials: make(map[string]string),
		width: width, height: height,
	}
	if m.status.RoleOverrides == nil {
		m.status.RoleOverrides = make(map[string]ModelManagerSubmission)
	}
	if m.width <= 0 {
		m.width = 80
	}
	if m.height <= 0 {
		m.height = 24
	}
	return m
}

func (m *ModelManager) Resize(width, height int) {
	if width > 0 {
		m.width = width
	}
	if height > 0 {
		m.height = height
	}
}

// SetRouteValidation records the daemon-owned automatic probe result for one
// completed selection. A failure remains visible and the draft stays editable.
func (m *ModelManager) SetRouteValidation(route string, ok bool, message, credentialStage string) {
	route = normalizeManagerRoute(route)
	if ok {
		if stage := strings.TrimSpace(credentialStage); stage != "" {
			m.credentialStage = stage
		}
		m.validation[route] = "validated"
		validated, exists := m.draft[route]
		if !exists || strings.TrimSpace(validated.APIKey) == "" {
			return
		}
		provider := validated.Provider
		for draftRoute, submission := range m.draft {
			if submission.Provider == provider {
				submission.APIKey = ""
				m.draft[draftRoute] = submission
			}
		}
		delete(m.credentials, provider)
		for index := range m.providers {
			if m.providers[index].ID == provider {
				m.providers[index].CredentialReady = true
			}
		}
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "validation failed"
	}
	m.validation[route] = message
}

// CredentialStage returns the daemon-issued opaque handle for credentials
// already validated in this draft. It contains no secret material.
func (m *ModelManager) CredentialStage() string { return m.credentialStage }

func (m *ModelManager) Update(msg tea.KeyMsg) ModelManagerAction {
	if m.status.RecoveryRequired {
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return ModelManagerAction{Closed: true}
		case "r":
			return ModelManagerAction{Closed: true, RecoveryAction: "retry"}
		case "b":
			return ModelManagerAction{Closed: true, RecoveryAction: "restore"}
		}
		return ModelManagerAction{}
	}
	if m.editingCustomModel {
		return m.updateCustomModel(msg)
	}
	if m.screen == modelScreenConnectionName || m.screen == modelScreenConnectionURL {
		return m.updateConnectionInput(msg)
	}
	if m.screen == modelScreenCredential {
		return m.updateCredential(msg)
	}
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return ModelManagerAction{Closed: true}
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "left", "backspace":
		m.back()
	case "enter":
		return m.choose()
	}
	return ModelManagerAction{}
}

func (m *ModelManager) updateCredential(msg tea.KeyMsg) ModelManagerAction {
	switch msg.String() {
	case "esc", "left":
		m.credentialInput = nil
		m.screen, m.index = modelScreenProvider, m.provider
	case "enter":
		key := strings.TrimSpace(string(m.credentialInput))
		if key == "" {
			return ModelManagerAction{}
		}
		m.credentials[m.currentProvider().ID] = key
		m.credentialInput = nil
		m.screen, m.index = modelScreenModel, m.model
	case "backspace":
		if len(m.credentialInput) > 0 {
			m.credentialInput = m.credentialInput[:len(m.credentialInput)-1]
		}
	default:
		if len(msg.Runes) > 0 {
			m.credentialInput = append(m.credentialInput, msg.Runes...)
		}
	}
	return ModelManagerAction{}
}

func (m *ModelManager) updateCustomModel(msg tea.KeyMsg) ModelManagerAction {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.editingCustomModel = false
		m.customModelInput = nil
	case "enter":
		id := strings.TrimSpace(string(m.customModelInput))
		if id != "" && len(m.providers) > 0 {
			provider := &m.providers[m.provider]
			provider.Models = append(provider.Models, ModelManagerModel{ID: id})
			m.model = len(provider.Models) - 1
			m.editingCustomModel = false
			m.customModelInput = nil
			m.screen = modelScreenReasoning
			m.index = 0
		}
	case "backspace":
		if len(m.customModelInput) > 0 {
			m.customModelInput = m.customModelInput[:len(m.customModelInput)-1]
		}
	default:
		if len(msg.Runes) > 0 {
			m.customModelInput = append(m.customModelInput, msg.Runes...)
		}
	}
	return ModelManagerAction{}
}

func (m *ModelManager) choose() ModelManagerAction {
	switch m.screen {
	case modelScreenMenu:
		switch m.index {
		case 0:
			m.beginRoute("primary")
		case 1:
			m.beginRoute("background")
		case 2:
			m.screen, m.index = modelScreenRoles, 0
		case 3:
			m.screen, m.index = modelScreenConnections, 0
		case 4:
			m.screen, m.index = modelScreenStatus, 0
		case 5:
			if len(m.draft) > 0 || len(m.providerDraft) > 0 {
				m.screen, m.index = modelScreenReview, 0
			}
		case 6:
			return ModelManagerAction{Closed: true}
		}
	case modelScreenRoles:
		if m.index >= len(modelManagerRoles) {
			m.screen, m.index = modelScreenMenu, 2
			break
		}
		m.roleIndex = m.index
		m.route = modelManagerRoles[m.roleIndex]
		m.screen, m.index = modelScreenRoleChoice, 0
	case modelScreenRoleChoice:
		switch m.index {
		case 0:
			m.setDraft(ModelManagerSubmission{Route: m.route, Reset: true})
			m.screen, m.index = modelScreenMenu, 2
			return m.draftAction(m.route)
		case 1:
			m.beginRoute(m.route)
		default:
			m.screen, m.index = modelScreenRoles, m.roleIndex
		}
	case modelScreenProvider:
		if m.route == "background" && m.index >= len(m.providers) {
			disabled := false
			selection := m.configuredSelection("background")
			selection.Enabled = &disabled
			m.setDraft(selection)
			m.screen, m.index = modelScreenMenu, 1
			return m.draftAction(m.route)
		}
		if len(m.providers) == 0 {
			return ModelManagerAction{}
		}
		m.provider, m.model, m.reasoning, m.serviceTier = m.index, 0, 0, 0
		m.alignModelOptions()
		provider := m.currentProvider()
		if provider.CredentialRequired && !provider.CredentialReady && strings.TrimSpace(m.credentials[provider.ID]) == "" {
			m.credentialInput = nil
			m.screen, m.index = modelScreenCredential, 0
		} else {
			m.screen, m.index = modelScreenModel, m.model
		}
	case modelScreenModel:
		provider := m.currentProvider()
		if m.index >= len(provider.Models) {
			m.editingCustomModel = true
			m.customModelInput = nil
			return ModelManagerAction{}
		}
		m.model = m.index
		m.alignTuningOptions()
		m.screen, m.index = modelScreenReasoning, m.reasoning
	case modelScreenReasoning:
		m.reasoning = m.index
		m.screen, m.index = modelScreenServiceTier, m.serviceTier
	case modelScreenServiceTier:
		m.serviceTier = m.index
		submission := m.currentSubmission()
		m.setDraft(submission)
		menuIndex := 2
		if m.route == "primary" {
			menuIndex = 0
		} else if m.route == "background" {
			menuIndex = 1
		}
		m.screen, m.index = modelScreenMenu, menuIndex
		return m.draftAction(m.route)
	case modelScreenReview:
		return ModelManagerAction{Closed: true, Draft: m.Draft(), ProviderDraft: m.ProviderDraft()}
	case modelScreenStatus:
		m.screen, m.index = modelScreenMenu, 4
	case modelScreenConnections:
		return m.chooseConnection()
	case modelScreenConnectionDetail:
		return m.chooseConnectionDetail()
	case modelScreenConnectionProtocol:
		m.connectionEditing.Protocol = []string{"openai-compatible", "anthropic-compatible", "responses-compatible"}[m.index]
		m.screen, m.index = modelScreenConnectionAuth, 0
	case modelScreenConnectionAuth:
		m.connectionEditing.Auth = []string{"bearer", "x-api-key", "none"}[m.index]
		m.finishConnectionEdit()
	}
	return ModelManagerAction{}
}

func (m *ModelManager) beginRoute(route string) {
	m.route = normalizeManagerRoute(route)
	m.alignToSelection(m.effectiveSelection(m.route))
	m.screen, m.index = modelScreenProvider, m.provider
}

func (m *ModelManager) setDraft(submission ModelManagerSubmission) {
	submission.Route = normalizeManagerRoute(submission.Route)
	baseline := m.configuredSelection(submission.Route)
	if submissionsEqual(submission, baseline) && strings.TrimSpace(submission.APIKey) == "" {
		delete(m.draft, submission.Route)
		delete(m.validation, submission.Route)
		return
	}
	m.draft[submission.Route] = submission
	m.validation[submission.Route] = "validating…"
}

func submissionsEqual(a, b ModelManagerSubmission) bool {
	return a.Route == b.Route && a.Provider == b.Provider && a.Model == b.Model &&
		normalizeAutoOption(a.Reasoning) == normalizeAutoOption(b.Reasoning) &&
		normalizeAutoOption(a.ServiceTier) == normalizeAutoOption(b.ServiceTier) && a.Reset == b.Reset &&
		submissionEnabled(a) == submissionEnabled(b)
}

func submissionEnabled(selection ModelManagerSubmission) bool {
	return selection.Enabled == nil || *selection.Enabled
}

func (m *ModelManager) Draft() []ModelManagerSubmission {
	order := append([]string{"primary", "background"}, modelManagerRoles...)
	out := make([]ModelManagerSubmission, 0, len(m.draft))
	for _, route := range order {
		if submission, ok := m.draft[route]; ok {
			out = append(out, submission)
		}
	}
	return out
}

func (m *ModelManager) ProviderDraft() []ModelManagerProviderSubmission {
	ids := make([]string, 0, len(m.providerDraft))
	for id := range m.providerDraft {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelManagerProviderSubmission, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.providerDraft[id])
	}
	return out
}

func (m *ModelManager) draftAction(route string) ModelManagerAction {
	return ModelManagerAction{Draft: m.Draft(), ProviderDraft: m.ProviderDraft(), ValidationRoute: route}
}

func (m *ModelManager) chooseConnection() ModelManagerAction {
	if m.index < len(m.providers) {
		m.connection = m.index
		provider := m.providers[m.connection]
		m.connectionEditing = ModelManagerProviderSubmission{
			ID: provider.ID, Custom: provider.Custom, BaseURL: provider.BaseURL,
			Protocol: provider.Protocol, Auth: provider.Auth,
		}
		m.connectionNew = false
		m.screen, m.index = modelScreenConnectionDetail, 0
		return ModelManagerAction{}
	}
	if m.index == len(m.providers) {
		m.connectionNew = true
		m.connectionEditing = ModelManagerProviderSubmission{Custom: true, Protocol: "openai-compatible", Auth: "bearer"}
		m.connectionInput = nil
		m.screen, m.index = modelScreenConnectionName, 0
		return ModelManagerAction{}
	}
	m.screen, m.index = modelScreenMenu, 3
	return ModelManagerAction{}
}

func (m *ModelManager) chooseConnectionDetail() ModelManagerAction {
	if m.index == 0 {
		m.connectionInput = []rune(m.connectionEditing.BaseURL)
		m.screen, m.index = modelScreenConnectionURL, 0
		return ModelManagerAction{}
	}
	if m.connectionEditing.Custom && m.index == 1 {
		deleted := m.connectionEditing
		deleted.Delete = true
		m.providerDraft[deleted.ID] = deleted
		for index := range m.providers {
			if strings.EqualFold(m.providers[index].ID, deleted.ID) {
				m.providers = append(m.providers[:index], m.providers[index+1:]...)
				break
			}
		}
		m.screen, m.index = modelScreenConnections, 0
		return ModelManagerAction{ProviderDraft: m.ProviderDraft()}
	}
	m.screen, m.index = modelScreenConnections, min(m.connection, len(m.providers))
	return ModelManagerAction{}
}

func (m *ModelManager) updateConnectionInput(msg tea.KeyMsg) ModelManagerAction {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.connectionInput = nil
		if m.connectionNew && m.screen == modelScreenConnectionName {
			m.screen, m.index = modelScreenConnections, len(m.providers)
		} else {
			m.screen, m.index = modelScreenConnectionDetail, 0
		}
	case "enter":
		value := strings.TrimSpace(string(m.connectionInput))
		if m.screen == modelScreenConnectionName {
			id := normalizeConnectionID(value)
			if id == "" || m.providerIDExists(id) {
				return ModelManagerAction{}
			}
			m.connectionEditing.ID = id
			m.connectionInput = nil
			m.screen = modelScreenConnectionURL
			return ModelManagerAction{}
		}
		if m.connectionEditing.Custom && value == "" {
			return ModelManagerAction{}
		}
		m.connectionEditing.BaseURL = strings.TrimRight(value, "/")
		m.connectionInput = nil
		if m.connectionEditing.Custom {
			m.screen, m.index = modelScreenConnectionProtocol, optionIndex([]string{"openai-compatible", "anthropic-compatible", "responses-compatible"}, m.connectionEditing.Protocol)
		} else {
			m.finishConnectionEdit()
		}
	case "backspace":
		if len(m.connectionInput) > 0 {
			m.connectionInput = m.connectionInput[:len(m.connectionInput)-1]
		}
	default:
		if len(msg.Runes) > 0 {
			m.connectionInput = append(m.connectionInput, msg.Runes...)
		}
	}
	return ModelManagerAction{}
}

func (m *ModelManager) finishConnectionEdit() {
	connection := m.connectionEditing
	connection.Delete = false
	m.providerDraft[connection.ID] = connection
	updated := false
	for index := range m.providers {
		if strings.EqualFold(m.providers[index].ID, connection.ID) {
			m.providers[index].BaseURL = connection.BaseURL
			m.providers[index].Protocol = connection.Protocol
			m.providers[index].Auth = connection.Auth
			m.providers[index].CredentialRequired = connection.Auth != "none"
			updated = true
			break
		}
	}
	if !updated {
		m.providers = append(m.providers, ModelManagerProvider{
			ID: connection.ID, Label: connection.ID, Custom: true,
			BaseURL: connection.BaseURL, Protocol: connection.Protocol, Auth: connection.Auth,
			CredentialRequired: connection.Auth != "none", CredentialReady: connection.Auth == "none",
		})
	}
	m.connectionNew = false
	m.screen, m.index = modelScreenConnections, 0
}

func (m *ModelManager) providerIDExists(id string) bool {
	for _, provider := range m.providers {
		if strings.EqualFold(provider.ID, id) {
			return true
		}
	}
	return false
}

func normalizeConnectionID(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
}

func (m *ModelManager) currentSubmission() ModelManagerSubmission {
	provider := m.currentProvider()
	model := m.currentModel()
	submission := ModelManagerSubmission{
		Route: m.route, Provider: provider.ID, Model: model.ID,
		Reasoning:   m.option(m.reasoningOptions(), m.reasoning),
		ServiceTier: m.option(m.serviceTierOptions(), m.serviceTier),
		APIKey:      m.credentials[provider.ID],
	}
	if m.route == "background" {
		enabled := true
		submission.Enabled = &enabled
	}
	return submission
}

func (m *ModelManager) option(options []string, index int) string {
	if index >= 0 && index < len(options) {
		return options[index]
	}
	return "auto"
}

func (m *ModelManager) effectiveSelection(route string) ModelManagerSubmission {
	if draft, ok := m.draft[normalizeManagerRoute(route)]; ok && !draft.Reset {
		return draft
	}
	return m.configuredSelection(route)
}

func (m *ModelManager) configuredSelection(route string) ModelManagerSubmission {
	route = normalizeManagerRoute(route)
	switch route {
	case "primary":
		return ModelManagerSubmission{Route: route, Provider: m.status.PrimaryProvider, Model: m.status.PrimaryModel, Reasoning: autoOption(m.status.PrimaryReasoning), ServiceTier: autoOption(m.status.PrimaryServiceTier)}
	case "background":
		enabled := m.status.BackgroundEnabled
		return ModelManagerSubmission{Route: route, Provider: m.status.BackgroundProvider, Model: m.status.BackgroundModel, Reasoning: autoOption(m.status.BackgroundReasoning), ServiceTier: autoOption(m.status.BackgroundServiceTier), Enabled: &enabled}
	default:
		if selection, ok := m.status.RoleOverrides[route]; ok {
			selection.Route = route
			selection.Reasoning = autoOption(selection.Reasoning)
			selection.ServiceTier = autoOption(selection.ServiceTier)
			return selection
		}
		return ModelManagerSubmission{Route: route, Reset: true}
	}
}

func (m *ModelManager) alignToSelection(selection ModelManagerSubmission) {
	m.provider, m.model, m.reasoning, m.serviceTier = 0, 0, 0, 0
	for i := range m.providers {
		if strings.EqualFold(m.providers[i].ID, selection.Provider) {
			m.provider = i
			break
		}
	}
	provider := m.currentProvider()
	for i := range provider.Models {
		if strings.EqualFold(provider.Models[i].ID, selection.Model) {
			m.model = i
			break
		}
	}
	m.reasoning = optionIndex(m.reasoningOptions(), selection.Reasoning)
	m.serviceTier = optionIndex(m.serviceTierOptions(), selection.ServiceTier)
}

func (m *ModelManager) alignModelOptions() {
	selection := m.effectiveSelection(m.route)
	if !strings.EqualFold(m.currentProvider().ID, selection.Provider) {
		return
	}
	for i := range m.currentProvider().Models {
		if strings.EqualFold(m.currentProvider().Models[i].ID, selection.Model) {
			m.model = i
			break
		}
	}
	m.alignTuningOptions()
}

func (m *ModelManager) alignTuningOptions() {
	selection := m.effectiveSelection(m.route)
	if strings.EqualFold(m.currentProvider().ID, selection.Provider) && strings.EqualFold(m.currentModel().ID, selection.Model) {
		m.reasoning = optionIndex(m.reasoningOptions(), selection.Reasoning)
		m.serviceTier = optionIndex(m.serviceTierOptions(), selection.ServiceTier)
		return
	}
	m.reasoning, m.serviceTier = 0, 0
}

func (m *ModelManager) back() {
	switch m.screen {
	case modelScreenMenu:
		return
	case modelScreenRoles:
		m.screen, m.index = modelScreenMenu, 2
	case modelScreenRoleChoice:
		m.screen, m.index = modelScreenRoles, m.roleIndex
	case modelScreenProvider:
		if isManagerRole(m.route) {
			m.screen, m.index = modelScreenRoleChoice, 1
		} else {
			m.screen = modelScreenMenu
			if m.route == "background" {
				m.index = 1
			}
		}
	case modelScreenModel:
		m.screen, m.index = modelScreenProvider, m.provider
	case modelScreenReasoning:
		m.screen, m.index = modelScreenModel, m.model
	case modelScreenServiceTier:
		m.screen, m.index = modelScreenReasoning, m.reasoning
	case modelScreenReview:
		m.screen, m.index = modelScreenMenu, 5
	case modelScreenStatus:
		m.screen, m.index = modelScreenMenu, 4
	case modelScreenConnections:
		m.screen, m.index = modelScreenMenu, 3
	case modelScreenConnectionDetail:
		m.screen, m.index = modelScreenConnections, m.connection
	case modelScreenConnectionProtocol:
		m.connectionInput = []rune(m.connectionEditing.BaseURL)
		m.screen, m.index = modelScreenConnectionURL, 0
	case modelScreenConnectionAuth:
		m.screen, m.index = modelScreenConnectionProtocol, 0
	}
}

func (m *ModelManager) move(delta int) {
	count := len(m.options())
	if count == 0 {
		m.index = 0
		return
	}
	m.index = (m.index + delta + count) % count
}

func (m *ModelManager) options() []string {
	switch m.screen {
	case modelScreenMenu:
		return []string{
			"Main model — " + m.routeSummary("primary"),
			"Background model — " + m.routeSummary("background"),
			fmt.Sprintf("Role overrides — %d explicit", m.explicitRoleCount()),
			fmt.Sprintf("Provider connections — %d custom", m.customProviderCount()),
			"Change status",
			fmt.Sprintf("Review and apply — %d change(s)", len(m.draft)+len(m.providerDraft)),
			"Exit",
		}
	case modelScreenRoles:
		out := make([]string, 0, len(modelManagerRoles)+1)
		for _, role := range modelManagerRoles {
			out = append(out, role+" — "+m.routeSummary(role))
		}
		return append(out, "Back")
	case modelScreenRoleChoice:
		return []string{"Use background model", "Choose an explicit model", "Back"}
	case modelScreenProvider:
		out := make([]string, 0, len(m.providers)+1)
		for _, provider := range m.providers {
			label := provider.Label
			if label == "" {
				label = provider.ID
			}
			if source := strings.TrimSpace(provider.Source); source != "" {
				out = append(out, fmt.Sprintf("%s (%s) · %s", label, provider.ID, source))
			} else {
				out = append(out, fmt.Sprintf("%s (%s)", label, provider.ID))
			}
		}
		if m.route == "background" {
			out = append(out, "Disable background model work")
		}
		return out
	case modelScreenModel:
		provider := m.currentProvider()
		out := make([]string, 0, len(provider.Models)+1)
		for _, model := range provider.Models {
			out = append(out, model.ID)
		}
		return append(out, "Enter a model ID manually…")
	case modelScreenReasoning:
		return m.reasoningOptions()
	case modelScreenServiceTier:
		return m.serviceTierOptions()
	case modelScreenReview:
		return []string{"Apply all changes"}
	case modelScreenStatus:
		return []string{"Back"}
	case modelScreenConnections:
		out := make([]string, 0, len(m.providers)+2)
		for _, provider := range m.providers {
			kind := "built-in"
			if provider.Custom {
				kind = "custom"
			}
			state := strings.TrimSpace(provider.BaseURL)
			if state == "" {
				state = "defaults"
			}
			out = append(out, fmt.Sprintf("%s — %s · %s", provider.ID, kind, state))
		}
		return append(out, "Add custom connection…", "Back")
	case modelScreenConnectionDetail:
		if m.connectionEditing.Custom {
			return []string{"Edit connection", "Delete connection", "Back"}
		}
		return []string{"Edit base URL override", "Back"}
	case modelScreenConnectionProtocol:
		return []string{"openai-compatible", "anthropic-compatible", "responses-compatible"}
	case modelScreenConnectionAuth:
		return []string{"bearer", "x-api-key", "none"}
	default:
		return nil
	}
}

func (m *ModelManager) customProviderCount() int {
	count := 0
	for _, provider := range m.providers {
		if provider.Custom {
			count++
		}
	}
	return count
}

func (m *ModelManager) routeSummary(route string) string {
	selection, changed := m.draft[normalizeManagerRoute(route)]
	if !changed {
		selection = m.configuredSelection(route)
	}
	if selection.Reset {
		return "Uses background model"
	}
	if route == "background" && !submissionEnabled(selection) {
		return "disabled"
	}
	label := strings.Trim(strings.TrimSpace(selection.Provider)+"/"+strings.TrimSpace(selection.Model), "/")
	if label == "" {
		label = "not configured"
	}
	if state := m.validation[normalizeManagerRoute(route)]; state != "" {
		label += " · " + state
	}
	return label
}

func (m *ModelManager) explicitRoleCount() int {
	count := 0
	for _, role := range modelManagerRoles {
		selection := m.configuredSelection(role)
		if draft, ok := m.draft[role]; ok {
			selection = draft
		}
		if !selection.Reset && (selection.Provider != "" || selection.Model != "") {
			count++
		}
	}
	return count
}

func (m *ModelManager) View() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Color(uitheme.TextPrimary))
	accent := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Color(uitheme.Accent))
	muted := lipgloss.NewStyle().Foreground(m.theme.Color(uitheme.TextSecondary))
	selected := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Color(uitheme.SelectionText)).Background(m.theme.Color(uitheme.SelectionBackground))
	lines := []string{"", title.Render("Model Manager")}
	lines = append(lines,
		muted.Render("Running main:       ")+emptyAsDash(m.status.RunningPrimary),
		muted.Render("Running background: ")+emptyAsDash(m.status.RunningBackground),
	)
	if m.status.ReadinessDegraded {
		lines = append(lines, accent.Render("Readiness: foreground ready · background degraded"))
	}
	if m.status.Pending != "" {
		lines = append(lines, accent.Render("Pending: ")+m.status.Pending)
	}
	if m.status.RecoveryRequired {
		lines = append(lines, "", lipgloss.NewStyle().Bold(true).Foreground(m.theme.Color(uitheme.Error)).Render("Gateway recovery required"))
		if failure := strings.TrimSpace(m.status.RecoveryFailure); failure != "" {
			lines = append(lines, muted.Render(failure))
		}
		return strings.Join(append(lines, "", "  r  retry the committed candidate", "  b  restore the last healthy model", "", muted.Render("r/b choose  Esc close")), "\n")
	}
	lines = append(lines, "", accent.Render(m.screenTitle()))
	if m.editingCustomModel {
		return strings.Join(append(lines, "", "  "+string(m.customModelInput)+"█", "", muted.Render("Enter save  Esc cancel")), "\n")
	}
	if m.screen == modelScreenCredential {
		masked := strings.Repeat("•", len(m.credentialInput))
		return strings.Join(append(lines, "", "  API key: "+masked+"█", "", muted.Render("Enter continue  Esc back")), "\n")
	}
	if m.screen == modelScreenConnectionName || m.screen == modelScreenConnectionURL {
		label := "Connection ID"
		if m.screen == modelScreenConnectionURL {
			label = "Base URL"
		}
		return strings.Join(append(lines, "", "  "+label+": "+string(m.connectionInput)+"█", "", muted.Render("Enter continue  Esc back")), "\n")
	}
	if m.screen == modelScreenReview {
		for _, provider := range m.ProviderDraft() {
			value := provider.BaseURL
			if provider.Delete {
				value = "Delete"
			}
			lines = append(lines, fmt.Sprintf("  %-20s %s", "provider "+provider.ID, value))
		}
		for _, selection := range m.Draft() {
			value := selection.Provider + "/" + selection.Model
			if selection.Reset {
				value = "Uses background model"
			} else if selection.Route == "background" && !submissionEnabled(selection) {
				value = "Disabled"
			}
			lines = append(lines, fmt.Sprintf("  %-20s %s", selection.Route, value))
			if state := m.validation[selection.Route]; state != "" {
				lines = append(lines, muted.Render("    "+state))
			}
		}
		lines = append(lines, "")
	}
	if m.screen == modelScreenStatus {
		lines = append(lines,
			"  Configured main:       "+emptyAsDash(m.status.ConfiguredPrimary),
			"  Configured background: "+emptyAsDash(m.status.ConfiguredBackground),
			fmt.Sprintf("  Generation:            %d", m.status.Generation),
			"  Foreground readiness:  "+managerReadinessLabel(m.status.ForegroundReady, m.status.ForegroundReason),
			"  Background readiness:  "+managerBackgroundReadinessLabel(m.status),
			"",
		)
	}
	for i, option := range m.options() {
		line := "  " + option
		if i == m.index {
			line = selected.Render("› " + option)
		}
		lines = append(lines, line)
	}
	footer := "↑/↓ choose  Enter continue  ← back  Esc close"
	if m.screen == modelScreenMenu {
		footer = "↑/↓ choose  Enter open  Esc close"
	}
	return strings.Join(append(lines, "", muted.Render(footer)), "\n")
}

func managerReadinessLabel(ready bool, reason string) string {
	if ready {
		return "ready"
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		return "not ready — " + reason
	}
	return "not ready"
}

func managerBackgroundReadinessLabel(status ModelManagerStatus) string {
	if !status.BackgroundEnabled {
		return "disabled"
	}
	return managerReadinessLabel(status.BackgroundReady, status.BackgroundReason)
}

func (m *ModelManager) screenTitle() string {
	switch m.screen {
	case modelScreenMenu:
		return "Settings"
	case modelScreenRoles:
		return "Role overrides (optional)"
	case modelScreenRoleChoice:
		return m.route
	case modelScreenProvider:
		return "Choose provider for " + m.route
	case modelScreenCredential:
		return "Enter API key for " + m.currentProvider().ID
	case modelScreenModel:
		return "Choose model"
	case modelScreenReasoning:
		return "Choose reasoning"
	case modelScreenServiceTier:
		return "Choose service tier"
	case modelScreenReview:
		return "Review one atomic change"
	case modelScreenStatus:
		return "Change status / recovery"
	case modelScreenConnections:
		return "Provider connections"
	case modelScreenConnectionDetail:
		return "Provider " + m.connectionEditing.ID
	case modelScreenConnectionName:
		return "Add custom connection"
	case modelScreenConnectionURL:
		return "Connection endpoint"
	case modelScreenConnectionProtocol:
		return "Compatible protocol"
	case modelScreenConnectionAuth:
		return "Authentication"
	default:
		return "Settings"
	}
}

func (m *ModelManager) reasoningOptions() []string {
	options := []string{"auto"}
	for _, value := range m.currentModel().Reasoning {
		if value = strings.TrimSpace(value); value != "" && !strings.EqualFold(value, "auto") {
			options = append(options, value)
		}
	}
	return uniqueManagerOptions(options)
}

func (m *ModelManager) serviceTierOptions() []string {
	options := []string{"auto"}
	for _, value := range m.currentModel().ServiceTiers {
		if value = strings.TrimSpace(value); value != "" && !strings.EqualFold(value, "auto") {
			options = append(options, value)
		}
	}
	return uniqueManagerOptions(options)
}

func uniqueManagerOptions(values []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func optionIndex(options []string, value string) int {
	value = autoOption(value)
	for i, option := range options {
		if strings.EqualFold(option, value) {
			return i
		}
	}
	return 0
}

func (m *ModelManager) currentProvider() ModelManagerProvider {
	if len(m.providers) == 0 {
		return ModelManagerProvider{}
	}
	if m.provider < 0 || m.provider >= len(m.providers) {
		m.provider = 0
	}
	return m.providers[m.provider]
}

func (m *ModelManager) currentModel() ModelManagerModel {
	provider := m.currentProvider()
	if len(provider.Models) == 0 {
		return ModelManagerModel{}
	}
	if m.model < 0 || m.model >= len(provider.Models) {
		m.model = 0
	}
	return provider.Models[m.model]
}

func normalizeManagerRoute(route string) string {
	route = strings.ToLower(strings.TrimSpace(route))
	if route == "auxiliary" {
		return "background"
	}
	return route
}

func isManagerRole(route string) bool {
	route = normalizeManagerRoute(route)
	for _, role := range modelManagerRoles {
		if route == role {
			return true
		}
	}
	return false
}

func autoOption(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "auto"
}

func normalizeAutoOption(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "auto") {
		return ""
	}
	return value
}

func emptyAsDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
