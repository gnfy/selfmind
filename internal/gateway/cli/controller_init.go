package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/components"
	"selfmind/internal/ui/components/sidebar"
	"selfmind/internal/ui/components/status"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func NewController(a *kernel.Agent, provider llm.Provider, cfg *config.Config, tenantID string) *Controller {
	if a != nil {
		a.SetEvolutionNotifyChannel(a.EventChannel)
	}
	c := &common.Common{
		Styles: common.DefaultStyles(),
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	var editorCfg *config.EditorConfig
	if cfg != nil {
		editorCfg = &cfg.Editor
	}
	if tenantID == "" {
		tenantID = "default"
	}
	editor := components.NewEditor(c, editorCfg)
	editor.SetCommandHints(slashCommandHints())
	channel := cliSessionChannel()
	historyStore, persistedHistory := newInputHistoryState(cfg)
	historyMaxBytes := int64(0)
	if cfg != nil {
		historyMaxBytes = cfg.History.MaxBytes
	}
	editor.SeedHistory(persistedHistory, historyMaxBytes)
	modelManagerStatus, modelManagerRoutes := buildModelManagerData(cfg)

	return &Controller{
		model: &uiModel{
			common:             c,
			sidebar:            sidebar.New(c),
			status:             status.New(c),
			editor:             editor,
			messages:           []ChatMessage{},
			thinking:           false,
			cursorVisible:      true,
			provider:           provider,
			agent:              a,
			tenantID:           tenantID,
			channel:            channel,
			spinner:            sp,
			inputHistoryStore:  historyStore,
			approvalMode:       "", // unset: requests omit the mode so the persisted /mode preference governs
			startTime:          time.Now(),
			runStatus:          "ready",
			tokenLimit:         resolveUITokenLimit(cfg, "", ""),
			modelMeta:          resolveUIModelMeta(cfg),
			modelManagerStatus: modelManagerStatus,
			modelManagerRoutes: modelManagerRoutes,
			clarifyBridge:      tools.NewClarifyBridge(),
		},
	}
}

func NewControllerWithGateway(gw *router.Gateway, agent *kernel.Agent, provider llm.Provider, providerName, modelName string, cfg *config.Config, tenantID string) *Controller {
	if agent != nil {
		agent.SetEvolutionNotifyChannel(agent.EventChannel)
	}
	c := &common.Common{
		Styles: common.DefaultStyles(),
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	var editorCfg *config.EditorConfig
	if cfg != nil {
		editorCfg = &cfg.Editor
	}
	if tenantID == "" {
		tenantID = "default"
	}
	editor := components.NewEditor(c, editorCfg)
	editor.SetCommandHints(slashCommandHints())
	channel := cliSessionChannel()
	historyStore, persistedHistory := newInputHistoryState(cfg)
	historyMaxBytes := int64(0)
	if cfg != nil {
		historyMaxBytes = cfg.History.MaxBytes
	}
	editor.SeedHistory(persistedHistory, historyMaxBytes)
	modelManagerStatus, modelManagerRoutes := buildModelManagerData(cfg)

	return &Controller{
		model: &uiModel{
			common:             c,
			sidebar:            sidebar.New(c),
			status:             status.New(c),
			editor:             editor,
			messages:           []ChatMessage{},
			thinking:           false,
			cursorVisible:      true,
			provider:           provider,
			providerName:       providerName,
			modelName:          modelName,
			agent:              agent,
			gateway:            gw,
			tenantID:           tenantID,
			channel:            channel,
			spinner:            sp,
			inputHistoryStore:  historyStore,
			approvalMode:       "", // unset: requests omit the mode so the persisted /mode preference governs
			startTime:          time.Now(),
			runStatus:          "ready",
			tokenLimit:         resolveUITokenLimit(cfg, providerName, modelName),
			modelMeta:          resolveUIModelMeta(cfg),
			modelManagerStatus: modelManagerStatus,
			modelManagerRoutes: modelManagerRoutes,
			clarifyBridge:      tools.NewClarifyBridge(),
		},
	}
}

func buildModelManagerData(cfg *config.Config) (components.ModelManagerStatus, []components.ModelManagerProvider) {
	if cfg == nil {
		return components.ModelManagerStatus{}, nil
	}
	snapshot := modelchange.SnapshotFromConfig(cfg)
	status := modelchange.Status{Running: snapshot, Configured: snapshot}
	if inspected, err := (&modelchange.Service{ConfigPath: cfg.Path}).Inspect(); err == nil {
		status = inspected
	}
	view := modelManagerStatusFrom(status)
	profiles := modelruntime.NewResolver(cfg).Registry().Profiles()
	activeProviders := map[string]bool{
		strings.ToLower(status.Configured.Primary.Provider):   true,
		strings.ToLower(status.Configured.Auxiliary.Provider): true,
	}
	for _, route := range modelchange.ManagedRoleRoutes() {
		selection := modelchange.SelectionForRoute(status.Configured, route)
		if provider := strings.ToLower(strings.TrimSpace(selection.Provider)); provider != "" {
			activeProviders[provider] = true
		}
	}
	resolver := modelruntime.NewResolver(cfg)
	catalog := modelruntime.NewCatalog(modelruntime.DefaultCatalogPath())
	configuredSelections := []config.ModelSelectionConfig{status.Configured.Primary, status.Configured.Auxiliary}
	for _, route := range modelchange.ManagedRoleRoutes() {
		configuredSelections = append(configuredSelections, modelchange.SelectionForRoute(status.Configured, route))
	}
	providers := make([]components.ModelManagerProvider, 0, len(profiles)+len(cfg.Providers.Custom))
	for _, profile := range profiles {
		models := append([]string(nil), profile.FallbackModels...)
		source := "built-in fallback"
		if activeProviders[strings.ToLower(profile.ID)] {
			selection := status.Configured.Primary
			for _, candidate := range configuredSelections {
				if strings.EqualFold(candidate.Provider, profile.ID) {
					selection = candidate
					break
				}
			}
			if runtime, err := resolver.Resolve(context.Background(), modelruntime.Selection{
				Provider: selection.Provider, Model: selection.Model,
			}); err == nil {
				probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				result := catalog.ModelsWithStatus(probeCtx, profile, runtime, false)
				cancel()
				if len(result.Models) > 0 {
					models = append(result.Models, models...)
					source = catalogSourceLabel(result)
				}
			}
		}
		for _, selection := range configuredSelections {
			if strings.EqualFold(selection.Provider, profile.ID) {
				models = prependUniqueModel(models, selection.Model)
			}
		}
		providers = append(providers, components.ModelManagerProvider{
			ID: profile.ID, Label: profile.DisplayName, Source: source,
			CredentialRequired: profile.AuthType == modelruntime.AuthAPIKey,
			CredentialReady:    modelManagerCredentialReady(resolver, profile.ID, models),
			Models:             modelManagerModels(profile.ID, models),
		})
	}
	for _, custom := range cfg.Providers.Custom {
		name := strings.TrimSpace(custom.Name)
		if name == "" {
			continue
		}
		models := []string{strings.TrimSpace(custom.Model)}
		providers = append(providers, components.ModelManagerProvider{
			ID: "custom:" + name, Label: name,
			CredentialRequired: true,
			CredentialReady:    modelManagerCredentialReady(resolver, "custom:"+name, models),
			Models:             modelManagerModels("custom:"+name, models),
		})
	}
	return view, providers
}

func modelManagerCredentialReady(resolver *modelruntime.Resolver, provider string, models []string) bool {
	if resolver == nil {
		return false
	}
	model := ""
	for _, candidate := range models {
		if model = strings.TrimSpace(candidate); model != "" {
			break
		}
	}
	runtime, err := resolver.Resolve(context.Background(), modelruntime.Selection{Provider: provider, Model: model})
	return err == nil && strings.TrimSpace(runtime.APIKey) != ""
}

func modelManagerStatusFrom(status modelchange.Status) components.ModelManagerStatus {
	view := components.ModelManagerStatus{
		RunningPrimary:        selectionDisplay(status.Running.Primary),
		RunningBackground:     selectionDisplay(status.Running.Auxiliary),
		ConfiguredPrimary:     selectionDisplay(status.Configured.Primary),
		ConfiguredBackground:  selectionDisplay(status.Configured.Auxiliary),
		PrimaryProvider:       status.Configured.Primary.Provider,
		PrimaryModel:          status.Configured.Primary.Model,
		PrimaryReasoning:      status.Configured.Primary.Reasoning,
		PrimaryServiceTier:    status.Configured.Primary.ServiceTier,
		BackgroundProvider:    status.Configured.Auxiliary.Provider,
		BackgroundModel:       status.Configured.Auxiliary.Model,
		BackgroundReasoning:   status.Configured.Auxiliary.Reasoning,
		BackgroundServiceTier: status.Configured.Auxiliary.ServiceTier,
		Generation:            status.Generation,
		RoleOverrides:         make(map[string]components.ModelManagerSubmission),
	}
	for _, route := range modelchange.ManagedRoleRoutes() {
		selection := modelchange.SelectionForRoute(status.Configured, route)
		if strings.TrimSpace(selection.Provider) == "" && strings.TrimSpace(selection.Model) == "" {
			continue
		}
		view.RoleOverrides[string(route)] = components.ModelManagerSubmission{
			Route: string(route), Provider: selection.Provider, Model: selection.Model,
			Reasoning: selection.Reasoning, ServiceTier: selection.ServiceTier,
		}
	}
	if status.Pending != nil {
		view.Pending = fmt.Sprintf("%s (%s)", status.Pending.ID, status.Pending.Status)
		if status.Pending.Status == modelchange.StatusRecoveryRequired {
			view.RecoveryRequired = true
			view.RecoveryFailure = status.Pending.Failure
		}
	}
	return view
}

func catalogSourceLabel(result modelruntime.CatalogResult) string {
	source := result.Source
	if !result.FetchedAt.IsZero() {
		source += ", fetched " + result.FetchedAt.Local().Format("2006-01-02 15:04")
	}
	if result.Stale {
		source += " (stale)"
	}
	return source
}

func modelManagerModels(provider string, ids []string) []components.ModelManagerModel {
	seen := map[string]bool{}
	models := make([]components.ModelManagerModel, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		entry := components.ModelManagerModel{ID: id}
		if descriptor, ok := modelruntime.DiscoverModelDescriptor(provider, id); ok {
			entry.Reasoning = append([]string(nil), descriptor.SupportedReasoning...)
			entry.ServiceTiers = append([]string(nil), descriptor.SupportedServiceTiers...)
		}
		models = append(models, entry)
	}
	return models
}

func prependUniqueModel(models []string, model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return models
	}
	for _, existing := range models {
		if strings.EqualFold(strings.TrimSpace(existing), model) {
			return models
		}
	}
	return append([]string{model}, models...)
}

func selectionDisplay(selection config.ModelSelectionConfig) string {
	provider := strings.TrimSpace(selection.Provider)
	model := strings.TrimSpace(selection.Model)
	if provider == "" {
		provider = "-"
	}
	if model == "" {
		model = "-"
	}
	reasoning := strings.TrimSpace(selection.Reasoning)
	if reasoning == "" {
		reasoning = "auto"
	}
	return fmt.Sprintf("%s/%s · reasoning=%s", provider, model, reasoning)
}

func (c *Controller) Start() {
	c.checkMigration()
	// Terminal-first hybrid is the ONLY renderer (the legacy alt-screen
	// viewport was removed with the in-process path, ACTIVE PLAN P0-3): run
	// inline so finalized cells commit to native scrollback, and without mouse
	// capture so the terminal owns selection/scroll.
	p := tea.NewProgram(c.model)
	c.model.program = p

	// Wrap p.Run() in a recovered goroutine so panics print to stderr and restore alt screen
	type result struct {
		model tea.Model
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				os.Stderr.WriteString("\x1b[?1049l")
				os.Stderr.WriteString(fmt.Sprintf("panic: %v\n", r))
				os.Exit(1)
			}
		}()
		val, err := p.Run()
		resCh <- result{val, err}
	}()
	// The watcher may immediately deliver an event. Start it only after the
	// Bubble Tea message loop is running so program.Send cannot stall startup.
	if c.model.eventWatcher != nil {
		ctx, cancel := context.WithCancel(context.Background())
		c.model.eventWatchCancel = cancel
		go c.model.eventWatcher(
			ctx,
			func(event llm.StreamEvent) {
				c.model.forwardGatewayEventFrom(event, eventSourceDaemon)
			},
			c.model.forwardDaemonRunEvent,
		)
	}
	res := <-resCh
	if c.model.eventWatchCancel != nil {
		c.model.eventWatchCancel()
		c.model.eventWatchCancel = nil
	}
	if res.err != nil {
		os.Stderr.WriteString("\x1b[?1049l")
		os.Stderr.WriteString(fmt.Sprintf("Error: %v\n", res.err))
		os.Exit(1)
	}
	if c.model.clarifyBridge != nil {
		c.model.clarifyBridge.Drain()
	}
	// Flush queued input-history writes before the process exits (writes are
	// async and best-effort; without this, the last inputs of a session could
	// be lost). No-op when persistence is disabled.
	c.model.inputHistoryStore.Close()
	if c.cleanupFn != nil {
		c.cleanupFn()
	}
}
