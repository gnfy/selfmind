package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/components"
	"selfmind/internal/ui/components/sidebar"
	"selfmind/internal/ui/components/status"
	uitheme "selfmind/internal/ui/theme"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// NewController builds the TUI controller. The TUI is a pure daemon client:
// chat, tools, and control commands all flow through the message processor
// installed by the caller (SetClientMode wiring); there is no in-process
// agent or gateway fallback.
func NewController(providerName, modelName string, cfg *config.Config, tenantID string) *Controller {
	c := common.New(ResolveTheme(cfg))
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(c.Theme.Color(uitheme.TextDecorative))

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
	tokenLimit, tokenLimitSource := resolveUIContext(cfg, providerName, modelName)

	return &Controller{
		model: &uiModel{
			common:             c,
			sidebar:            sidebar.New(c),
			status:             status.New(c),
			editor:             editor,
			messages:           []ChatMessage{},
			thinking:           false,
			cursorVisible:      true,
			providerName:       providerName,
			modelName:          modelName,
			tenantID:           tenantID,
			channel:            channel,
			spinner:            sp,
			inputHistoryStore:  historyStore,
			approvalMode:       "", // unset: requests omit the mode so the persisted /mode preference governs
			startTime:          time.Now(),
			runStatus:          "ready",
			tokenLimit:         tokenLimit,
			tokenLimitSource:   tokenLimitSource,
			modelMeta:          resolveUIModelMeta(cfg),
			modelManagerStatus: modelManagerStatus,
			modelManagerRoutes: modelManagerRoutes,
			clarifyBridge:      tools.NewClarifyBridge(),
			process:            newProcessSurface(),
		},
	}
}

// ResolveTheme shares the terminal appearance between startup and chat.
func ResolveTheme(cfg *config.Config) uitheme.Theme {
	configured := "auto"
	if cfg != nil {
		configured = cfg.TUI.Theme
	}
	mode, err := uitheme.ParseMode(configured)
	if err != nil {
		mode = uitheme.ModeAuto
	}
	profile := lipgloss.ColorProfile()
	dark := true
	if mode == uitheme.ModeAuto && profile != termenv.Ascii {
		dark = lipgloss.HasDarkBackground()
	} else if mode == uitheme.ModeLight {
		dark = false
	}
	resolved, err := uitheme.Resolve(uitheme.Options{
		Mode: mode, Profile: profile, DarkBackground: dark,
	})
	if err != nil {
		return uitheme.Default()
	}
	return resolved
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
		managerModels := modelManagerModels(profile.ID, models, cfg.Models.Remembered, configuredSelections)
		endpoint, hasEndpoint := cfg.Providers.BuiltinEndpoint(profile.ID)
		if !hasEndpoint {
			endpoint = cfg.ProviderProfiles[profile.ID]
		}
		providers = append(providers, components.ModelManagerProvider{
			ID: profile.ID, Label: profile.DisplayName, Source: source,
			CredentialRequired: profile.AuthType == modelruntime.AuthAPIKey,
			CredentialReady:    modelManagerCredentialReady(resolver, profile.ID, modelManagerModelIDs(managerModels)),
			Models:             managerModels,
			BaseURL:            endpoint.BaseURL,
			Protocol:           firstNonEmptyText(endpoint.Protocol, profile.Protocol),
		})
	}
	for _, custom := range cfg.Providers.Custom {
		name := strings.TrimSpace(custom.Name)
		if name == "" {
			continue
		}
		models := []string{strings.TrimSpace(custom.Model)}
		for model := range custom.Models {
			models = append(models, model)
		}
		managerModels := modelManagerModels(name, models, cfg.Models.Remembered, configuredSelections)
		providers = append(providers, components.ModelManagerProvider{
			ID: name, Label: name,
			Custom: true, BaseURL: custom.BaseURL, Protocol: custom.Protocol, Auth: custom.Auth,
			CredentialRequired: !strings.EqualFold(strings.TrimSpace(custom.Auth), "none"),
			CredentialReady:    strings.EqualFold(strings.TrimSpace(custom.Auth), "none") || modelManagerCredentialReady(resolver, name, modelManagerModelIDs(managerModels)),
			Models:             managerModels,
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
		RunningPrimary:           selectionDisplayWithTuning(status.Running.Primary, status.RunningTuning.Primary),
		RunningBackground:        selectionDisplayWithTuning(status.Running.Auxiliary, status.RunningTuning.Auxiliary),
		ConfiguredPrimary:        selectionDisplayWithTuning(status.Configured.Primary, status.ConfiguredTuning.Primary),
		ConfiguredBackground:     selectionDisplayWithTuning(status.Configured.Auxiliary, status.ConfiguredTuning.Auxiliary),
		PrimaryProvider:          status.Configured.Primary.Provider,
		PrimaryModel:             status.Configured.Primary.Model,
		PrimaryReasoning:         status.Configured.Primary.Reasoning,
		PrimaryReasoningLabel:    reasoningStatusLabel(status.Configured.Primary, status.ConfiguredTuning.Primary),
		PrimaryServiceTier:       status.Configured.Primary.ServiceTier,
		BackgroundProvider:       status.Configured.Auxiliary.Provider,
		BackgroundModel:          status.Configured.Auxiliary.Model,
		BackgroundReasoning:      status.Configured.Auxiliary.Reasoning,
		BackgroundReasoningLabel: reasoningStatusLabel(status.Configured.Auxiliary, status.ConfiguredTuning.Auxiliary),
		BackgroundServiceTier:    status.Configured.Auxiliary.ServiceTier,
		BackgroundEnabled:        status.Configured.Auxiliary.Enabled == nil || *status.Configured.Auxiliary.Enabled,
		BackgroundFollowsMain:    status.Configured.Auxiliary.FollowPrimary,
		ForegroundReady:          status.ForegroundReady(),
		BackgroundReady:          status.BackgroundReady(),
		ReadinessDegraded:        status.Readiness.Degraded,
		ForegroundReason:         status.Readiness.ForegroundReason,
		BackgroundReason:         status.Readiness.BackgroundReason,
		Generation:               status.Generation,
		RoleOverrides:            make(map[string]components.ModelManagerSubmission),
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

func modelManagerModels(provider string, availableIDs []string, remembered []config.RememberedModelConfig, configured []config.ModelSelectionConfig) []components.ModelManagerModel {
	available := make(map[string]bool, len(availableIDs))
	configuredModels := make(map[string]bool)
	rememberedModels := make(map[string]config.RememberedModelConfig)
	ids := make([]string, 0, len(availableIDs)+len(remembered)+len(configured))
	for _, selection := range configured {
		if strings.EqualFold(selection.Provider, provider) && strings.TrimSpace(selection.Model) != "" {
			ids = append(ids, selection.Model)
			configuredModels[strings.ToLower(strings.TrimSpace(selection.Model))] = true
		}
	}
	for _, id := range availableIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
			available[strings.ToLower(id)] = true
		}
	}
	for _, entry := range remembered {
		if strings.EqualFold(entry.Provider, provider) && strings.TrimSpace(entry.Model) != "" {
			ids = append(ids, entry.Model)
			rememberedModels[strings.ToLower(strings.TrimSpace(entry.Model))] = entry
		}
	}
	seen := map[string]bool{}
	models := make([]components.ModelManagerModel, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			continue
		}
		seen[key] = true
		entry := components.ModelManagerModel{
			ID: id, Available: available[key], Configured: configuredModels[key],
		}
		if descriptor, ok := modelruntime.DiscoverModelDescriptor(provider, id); ok {
			entry.Reasoning = append([]string(nil), descriptor.SupportedReasoning...)
			entry.ServiceTiers = append([]string(nil), descriptor.SupportedServiceTiers...)
			entry.AllowManualReasoning = len(descriptor.SupportedReasoning) == 0
		} else {
			entry.AllowManualReasoning = true
		}
		if recent, ok := rememberedModels[key]; ok {
			entry.Remembered = true
			entry.Reasoning = appendUniqueFold(entry.Reasoning, recent.Reasoning...)
		}
		models = append(models, entry)
	}
	return models
}

func modelManagerModelIDs(models []components.ModelManagerModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func appendUniqueFold(values []string, additions ...string) []string {
	for _, addition := range additions {
		addition = strings.TrimSpace(addition)
		if addition == "" {
			continue
		}
		found := false
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(value), addition) {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}

func selectionDisplay(selection config.ModelSelectionConfig) string {
	if selection.Enabled != nil && !*selection.Enabled {
		return "disabled"
	}
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

func selectionDisplayWithTuning(selection config.ModelSelectionConfig, tuning modelchange.RouteTuning) string {
	if selection.Enabled != nil && !*selection.Enabled {
		return "disabled"
	}
	provider := strings.TrimSpace(selection.Provider)
	model := strings.TrimSpace(selection.Model)
	if provider == "" {
		provider = "-"
	}
	if model == "" {
		model = "-"
	}
	return fmt.Sprintf("%s/%s · reasoning=%s", provider, model, reasoningStatusLabel(selection, tuning))
}

func reasoningStatusLabel(selection config.ModelSelectionConfig, tuning modelchange.RouteTuning) string {
	if explicit := strings.TrimSpace(selection.Reasoning); explicit != "" {
		return explicit
	}
	effective := strings.TrimSpace(tuning.Reasoning)
	if effective != "" {
		return "auto→" + effective
	}
	return "auto"
}

func (c *Controller) Start() {
	c.checkMigration()
	// Terminal-first hybrid is the ONLY renderer (the legacy alt-screen
	// viewport was removed with the in-process path, ACTIVE PLAN P0-3): run
	// inline so finalized cells commit to native scrollback, and without mouse
	// capture so the terminal owns selection/scroll.
	// Focus reporting drives the terminal attention signal: an approval that
	// parks the run stays silent while the person is demonstrably watching it.
	p := tea.NewProgram(c.model, tea.WithReportFocus(), tea.WithOutput(newTerminalCursorOutput(os.Stdout)))
	c.model.program = p
	stopHangupWatch := watchHangup(p)
	defer stopHangupWatch()

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
