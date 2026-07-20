package cli

import (
	"fmt"
	"os"
	"time"

	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
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
	historyStore, persistedHistory := newInputHistoryState(cfg, channel)

	return &Controller{
		model: &uiModel{
			common:            c,
			sidebar:           sidebar.New(c),
			status:            status.New(c),
			editor:            editor,
			messages:          []ChatMessage{},
			thinking:          false,
			cursorVisible:     true,
			provider:          provider,
			agent:             a,
			tenantID:          tenantID,
			channel:           channel,
			spinner:           sp,
			inputHistory:      persistedHistory,
			inputHistoryStore: historyStore,
			historyIndex:      -1,
			approvalMode:      "", // unset: requests omit the mode so the persisted /mode preference governs
			startTime:         time.Now(),
			runStatus:         "ready",
			tokenLimit:        resolveUITokenLimit(cfg, "", ""),
			modelMeta:         resolveUIModelMeta(cfg),
			clarifyBridge:     tools.NewClarifyBridge(),
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
	historyStore, persistedHistory := newInputHistoryState(cfg, channel)

	return &Controller{
		model: &uiModel{
			common:            c,
			sidebar:           sidebar.New(c),
			status:            status.New(c),
			editor:            editor,
			messages:          []ChatMessage{},
			thinking:          false,
			cursorVisible:     true,
			provider:          provider,
			providerName:      providerName,
			modelName:         modelName,
			agent:             agent,
			gateway:           gw,
			tenantID:          tenantID,
			channel:           channel,
			spinner:           sp,
			inputHistory:      persistedHistory,
			inputHistoryStore: historyStore,
			historyIndex:      -1,
			approvalMode:      "", // unset: requests omit the mode so the persisted /mode preference governs
			startTime:         time.Now(),
			runStatus:         "ready",
			tokenLimit:        resolveUITokenLimit(cfg, providerName, modelName),
			modelMeta:         resolveUIModelMeta(cfg),
			clarifyBridge:     tools.NewClarifyBridge(),
		},
	}
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
	res := <-resCh
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
