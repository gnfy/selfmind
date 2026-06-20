package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"selfmind/internal/app"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/components"
	"selfmind/internal/ui/components/sidebar"
	"selfmind/internal/ui/components/status"
	"selfmind/internal/ui/layout"
)

// Controller wraps the Bubble Tea program.
type Controller struct {
	model     *uiModel
	cleanupFn func()
}

// ChatMessage represents a single message in the conversation history.
type ChatMessage struct {
	Role      string // "user", "assistant", "system", "tool"
	Content   string
	Timestamp time.Time
	ToolName  string  // populated when Role == "tool"
	ToolArgs  string  // Fix: add ToolArgs to store call arguments
	Duration  float64 // Fix: add Duration for performance display
	IsError   bool    // Fix: add IsError flag
	IsRunning bool
}

// uiModel is the main TUI model. It holds all conversation state.
type uiModel struct {
	program            *tea.Program
	width, height      int
	common             *common.Common
	sidebar            *sidebar.Sidebar
	status             *status.Status
	editor             *components.Editor
	viewport           viewport.Model
	sessionBrowser     *components.SessionBrowser
	sessionBrowserOpen bool
	pager              *components.Pager
	messages           []ChatMessage
	thinking           bool
	toolExecuting      string
	totalTokens        int
	tokenLimit         int
	startTime          time.Time
	provider           llm.Provider
	providerName       string
	modelName          string
	agent              *kernel.Agent
	gateway            *router.Gateway
	tenantID           string
	channel            string // 'cli' | 'wechat' | 'dingtalk' | 'web'
	spinner            spinner.Model
	inputHistory       []string
	historyIndex       int
	historyDraft       string
	sessionSearchFn    func(query string, limit int) (interface{}, error)
	clarifyBridge      *tools.ClarifyBridge
	cancelFn           context.CancelFunc
	clarifyMode        bool
	clarifyChoices     []string
	clarifyReq         tools.ClarifyRequest
	secretMode         bool
	secretKey          string
	statusMsg          string    // Transient status message
	thinkingDots       int       // Counter for "..." animation
	thinkingStart      time.Time // When current thinking started
	runStatus          string    // ready | working | done | error | cancelled
	migrationHint      string    // Hint for migrating Hermes skills
}

type MsgClearStatus struct{}
type MsgWorkingTick time.Time

func workingTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return MsgWorkingTick(t)
	})
}

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

	return &Controller{
		model: &uiModel{
			common:        c,
			sidebar:       sidebar.New(c),
			status:        status.New(c),
			editor:        editor,
			messages:      []ChatMessage{},
			thinking:      false,
			provider:      provider,
			agent:         a,
			tenantID:      tenantID,
			channel:       "cli",
			spinner:       sp,
			inputHistory:  []string{},
			historyIndex:  -1,
			startTime:     time.Now(),
			runStatus:     "ready",
			tokenLimit:    resolveUITokenLimit(cfg, "", ""),
			viewport:      viewport.New(0, 0),
			clarifyBridge: tools.NewClarifyBridge(),
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

	return &Controller{
		model: &uiModel{
			common:        c,
			sidebar:       sidebar.New(c),
			status:        status.New(c),
			editor:        editor,
			messages:      []ChatMessage{},
			thinking:      false,
			provider:      provider,
			providerName:  providerName,
			modelName:     modelName,
			agent:         agent,
			gateway:       gw,
			tenantID:      tenantID,
			channel:       "cli",
			spinner:       sp,
			inputHistory:  []string{},
			historyIndex:  -1,
			startTime:     time.Now(),
			runStatus:     "ready",
			tokenLimit:    resolveUITokenLimit(cfg, providerName, modelName),
			viewport:      viewport.New(0, 0),
			clarifyBridge: tools.NewClarifyBridge(),
		},
	}
}

func (c *Controller) SetSessionSearchFn(fn func(query string, limit int) (interface{}, error)) {
	c.model.sessionSearchFn = fn
}

func (c *Controller) SetCheckpointFns(memFn func() (*memory.MemoryManager, string, string), msgFn func() ([]byte, error)) {
	SetCheckpointMemGetter(memFn)
	SetCheckpointMessagesFn(func() ([]ChatMessage, error) {
		raw, err := msgFn()
		if err != nil || raw == nil {
			return nil, err
		}
		var msgs []ChatMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, err
		}
		return msgs, nil
	})
}

func (c *Controller) SetCleanupFn(fn func()) {
	c.cleanupFn = fn
}

func (c *Controller) ClarifyHandler() tools.ClarifyHandler {
	if c == nil || c.model == nil || c.model.clarifyBridge == nil {
		return nil
	}
	return c.model.clarifyBridge.Handler()
}

func (c *Controller) checkMigration() {
	if !app.NeedsMigration() {
		return
	}

	c.model.migrationHint = "Type /migrate to import your Hermes skills."
}

func (c *Controller) Start() {
	c.checkMigration()
	p := tea.NewProgram(c.model,
		tea.WithAltScreen(),
	)
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
	if c.cleanupFn != nil {
		c.cleanupFn()
	}
}

func (m *uiModel) Init() tea.Cmd {
	return m.spinner.Tick
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func (m *uiModel) addMessage(role, content string) {
	m.messages = append(m.messages, ChatMessage{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	})
	m.viewport.GotoBottom()
}

func (m *uiModel) addErrorMessage(content string) {
	m.messages = append(m.messages, ChatMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now(),
		IsError:   true,
	})
	m.viewport.GotoBottom()
}

func (m *uiModel) appendToolOutput(toolName, content string) {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != "tool" {
		m.addMessage("tool", "")
	}
	last := &m.messages[len(m.messages)-1]
	if toolName != "" {
		last.ToolName = toolName
	}
	last.IsRunning = true
	if strings.TrimSpace(last.Content) == "" {
		last.Content = content
		return
	}
	last.Content += "\n" + content
}

func (m *uiModel) appendAssistantResponse(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if len(m.messages) > 0 {
		last := &m.messages[len(m.messages)-1]
		if last.Role == "assistant" && !last.IsError {
			existing := strings.TrimSpace(last.Content)
			switch {
			case existing == "":
				last.Content = content
				m.viewport.GotoBottom()
				return
			case existing == content:
				return
			case strings.HasSuffix(existing, content):
				return
			}
		}
	}
	m.addMessage("assistant", content)
}

func (m *uiModel) viewModel() string {
	if m.pager != nil {
		return m.pager.View()
	}

	mainW := m.width
	st := m.common.Styles

	wasAtBottom := m.viewport.AtBottom() || m.viewport.PastBottom()

	inputH := m.editor.PreferredHeight()
	inputRect := layout.Rect{W: m.width, H: inputH}
	inputArea := m.editor.Draw(inputRect)

	notification := m.notificationBar(mainW)
	migrationHint := m.migrationHintBar(mainW)

	// Codex-cli style: transcript + composer + one-line footer.
	visibleH := m.height - inputH - 1
	if notification != "" {
		visibleH--
	}
	if migrationHint != "" {
		visibleH--
	}
	if visibleH < 1 {
		visibleH = 1
	}

	m.viewport.Width = mainW
	m.viewport.Height = visibleH
	fullContent := m.renderAllMessages()
	stickToBottom := wasAtBottom || strings.TrimSpace(m.editor.Value()) != "" || m.thinking
	showStartup := len(m.messages) == 0 && !m.thinking
	m.viewport.SetContent(fullContent)
	if showStartup {
		m.viewport.GotoTop()
		m.viewport.YOffset = 0
	} else if stickToBottom {
		m.viewport.GotoBottom()
	}

	mainStr := st.Main.Width(mainW).Height(visibleH).Render(m.viewport.View())

	statusBar := st.Status.Panel.Width(m.width).Render(m.statusLine())

	return lipgloss.JoinVertical(lipgloss.Left, mainStr, notification, migrationHint, inputArea, statusBar)
}

func (m *uiModel) notificationBar(width int) string {
	if width <= 0 || strings.TrimSpace(m.statusMsg) == "" {
		return ""
	}
	text := strings.TrimSpace(m.statusMsg)
	style := lipgloss.NewStyle().
		Width(width).
		Padding(0, 1).
		Foreground(lipgloss.Color("255")).
		Background(lipgloss.Color("236"))
	if strings.Contains(strings.ToLower(text), "copied") {
		style = style.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("82")).Bold(true)
	}
	return style.Render(truncateToWidth(text, width-2))
}

func (m *uiModel) migrationHintBar(width int) string {
	if width <= 0 || m.migrationHint == "" {
		return ""
	}
	return lipgloss.NewStyle().
		Width(width).
		Padding(0, 1).
		Foreground(lipgloss.Color("212")).
		Background(lipgloss.Color("236")).
		Italic(true).
		Render(truncateToWidth("✨ "+m.migrationHint, width-2))
}

func (m *uiModel) openHelp() {
	width := m.width
	if width <= 0 {
		width = 80
	}
	m.pager = components.NewPager(m.common, width, m.height, m.renderHelpContent)
}

func (m *uiModel) renderHelpContent(width int) string {
	if width < 40 {
		width = 40
	}
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	section := lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	cmdStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	lines := []string{
		"",
		title.Render("SelfMind help"),
		muted.Render("Send tasks, inspect code, run tools, and manage what SelfMind learns."),
		"",
		section.Render("Keyboard shortcuts:"),
	}

	for _, row := range shortcutHelpRows {
		lines = append(lines, renderHelpRow(row.Left, row.Right, keyStyle, descStyle, width))
	}

	lines = append(lines, "", section.Render("Slash commands:"))
	for _, cmd := range slashCommandMetas {
		lines = append(lines, renderHelpRow(cmd.Usage, cmd.Description, cmdStyle, descStyle, width))
	}

	lines = append(lines, "", muted.Render("Press q or Esc to close this page."))
	return strings.Join(lines, "\n")
}

func renderHelpRow(left, right string, leftStyle, rightStyle lipgloss.Style, width int) string {
	leftW := 30
	if width < 72 {
		leftW = 24
	}
	descW := width - leftW - 5
	if descW < 12 {
		descW = 12
	}
	return "  " + leftStyle.Copy().Width(leftW).Render(left) + " " + rightStyle.Render(truncateToWidth(right, descW))
}

func (m *uiModel) statusLine() string {
	st := m.common.Styles
	header := m.displayModelName() + " default"
	cwd := currentWorkingDir()

	parts := []string{
		st.Status.Value.Render(header),
		st.Status.Value.Render(cwd),
		st.Status.Label.Render(formatUsage(m.totalTokens, m.tokenLimit)),
	}

	state := m.runStatus
	if m.thinking {
		elapsed := time.Since(m.thinkingStart).Seconds()
		state = fmt.Sprintf("working %.1fs", elapsed)
	}
	parts = append(parts, st.Status.Good.Render(state))

	if m.toolExecuting != "" {
		parts = append(parts, st.Status.Warning.Render(m.toolExecuting))
	}

	ctrlHint := "Ctrl+C exit"
	if m.thinking || m.toolExecuting != "" {
		ctrlHint = "Ctrl+C cancel"
	} else if strings.TrimSpace(m.editor.Value()) != "" {
		ctrlHint = "Ctrl+C clear"
	}
	parts = append(parts, st.Status.Label.Render(ctrlHint), st.Status.Label.Render("/help"))

	return strings.Join(parts, " · ")
}

func (m *uiModel) displayModelName() string {
	modelName := strings.TrimSpace(m.modelName)
	providerName := strings.TrimSpace(m.providerName)
	if modelName == "" {
		modelName = providerName
	}
	if modelName == "" {
		modelName = "active"
	}
	if providerName != "" && providerName != modelName && providerName != "active" && providerName != "not configured" {
		return providerName + "/" + modelName
	}
	return modelName
}

func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	limit := width - 3
	var sb strings.Builder
	used := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if used+rw > limit {
			break
		}
		sb.WriteRune(r)
		used += rw
	}
	return sb.String() + "..."
}

func currentWorkingDir() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "."
	}
	return cwd
}

func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if events := m.clarifyBridge.Events(); events != nil {
		select {
		case req := <-events:
			m.thinking = false
			m.toolExecuting = ""
			m.clarifyMode = true
			m.clarifyChoices = req.Choices
			m.addMessage("assistant", fmt.Sprintf("❓ %s", req.Question))
			if len(req.Choices) > 0 {
				var lines []string
				for i, c := range req.Choices {
					lines = append(lines, fmt.Sprintf("  %d. %s", i+1, c))
				}
				lines = append(lines, "  0. Other (type your answer)")
				m.addMessage("assistant", "Options:\n"+strings.Join(lines, "\n"))
			}
			m.clarifyReq = req
			return m, nil
		default:
		}
	}

	spinnerCmd := tea.Cmd(nil)
	if m.thinking {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		spinnerCmd = cmd

		// Animate "Thinking..." dots every ~500ms
		if _, ok := msg.(spinner.TickMsg); ok {
			m.thinkingDots = int(time.Since(m.thinkingStart).Seconds() * 2)
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.common.Width, m.common.Height = msg.Width, msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 5
		if m.pager != nil {
			m.pager.Resize(msg.Width, msg.Height)
		}
		return m, nil

	case spinner.TickMsg:
		return m, spinnerCmd

	case MsgWorkingTick:
		if m.thinking || m.toolExecuting != "" {
			m.thinkingDots++
			return m, workingTick()
		}
		return m, nil

	case tea.KeyMsg:
		if m.pager != nil {
			closed, cmd := m.pager.Update(msg)
			if closed {
				m.pager = nil
			}
			return m, cmd
		}
		return m.handleKey(msg)

	case tea.MouseMsg:
		if m.pager != nil {
			closed, cmd := m.pager.Update(msg)
			if closed {
				m.pager = nil
			}
			return m, cmd
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case MsgStream:
		m.thinking = false
		if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == "assistant" && !m.messages[len(m.messages)-1].IsError {
			// 如果最后一条消息是助手回复，且不是错误，则追加
			m.messages[len(m.messages)-1].Content += msg.Content
		} else {
			// 否则创建新的助手消息
			m.messages = append(m.messages, ChatMessage{
				Role:      "assistant",
				Content:   msg.Content,
				Timestamp: time.Now(),
			})
		}
		m.viewport.GotoBottom()
		return m, nil

	case MsgAgentDone:
		m.thinking = false
		m.toolExecuting = ""
		m.totalTokens += msg.Usage.InputTokens + msg.Usage.OutputTokens
		if msg.Err != nil {
			m.runStatus = "error"
			m.addErrorMessage(fmt.Sprintf("Error: %v", msg.Err))
		} else if strings.TrimSpace(msg.Response) != "" {
			m.runStatus = "done"
			m.appendAssistantResponse(msg.Response)
		} else {
			m.runStatus = "error"
			m.addErrorMessage("Error: model returned an empty response without any error details. Check the provider credentials and endpoint, then retry.")
		}
		return m, spinnerCmd

	case MsgToolStart:
		m.toolExecuting = msg.ToolName
		m.addMessage("tool", "")
		last := &m.messages[len(m.messages)-1]
		last.ToolName = msg.ToolName
		last.ToolArgs = msg.Args
		last.IsRunning = true
		return m, spinnerCmd

	case MsgToolDone:
		m.toolExecuting = ""
		if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == "tool" {
			last := &m.messages[len(m.messages)-1]
			last.ToolName = msg.ToolName
			last.Duration = msg.Duration
			last.IsRunning = false
			if msg.Err != nil {
				existing := strings.TrimSpace(last.Content)
				errText := fmt.Sprintf("%s error: %v", msg.ToolName, msg.Err)
				if existing != "" {
					last.Content = existing + "\n" + errText
				} else {
					last.Content = errText
				}
				last.IsError = true
			} else {
				if strings.TrimSpace(msg.Result) != "" {
					last.Content = msg.Result
				}
				last.IsError = false
			}
		}
		return m, spinnerCmd

	case MsgToolOutput:
		m.thinking = false
		m.appendToolOutput(msg.ToolName, msg.Content)
		m.viewport.GotoBottom()
		return m, spinnerCmd

	case MsgToolHeartbeat:
		if msg.ToolName != "" {
			m.toolExecuting = msg.ToolName
		}
		m.statusMsg = msg.Content
		return m, spinnerCmd

	case MsgLearningEvent:
		m.statusMsg = msg.Content
		m.addMessage("system", msg.Content)
		return m, nil

	case MsgClearStatus:
		m.statusMsg = ""
		return m, nil

	default:
		cmd := m.editor.Update(msg)
		return m, cmd
	}
}

func (m *uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {

	// Shift+Enter or Ctrl+J inserts a newline (multi-line input).
	case tea.KeyCtrlJ:
		m.editor.Update(msg)
		return m, nil
	case tea.KeyUp:
		if m.navigateInputHistory(-1) {
			return m, nil
		}
		m.editor.Update(msg)
		return m, nil
	case tea.KeyDown:
		if m.navigateInputHistory(1) {
			return m, nil
		}
		m.editor.Update(msg)
		return m, nil

	default:
		switch msg.String() {
		case "esc":
			return m, nil
		case "ctrl+c":
			// Priority 1: if input has content, clear it (don't quit)
			if input := m.editor.Value(); input != "" {
				m.editor.Reset()
				return m, nil
			}
			// Priority 2: if agent is thinking or tool running, cancel it
			if m.thinking || m.toolExecuting != "" {
				if m.cancelFn != nil {
					m.cancelFn()
					m.thinking = false
					m.toolExecuting = ""
					m.runStatus = "cancelled"
					m.statusMsg = "Task cancelled by user."
					return m, tea.Tick(time.Second*3, func(t time.Time) tea.Msg {
						return MsgClearStatus{}
					})
				}
				return m, nil
			}
			// Priority 3: quit
			return m, tea.Quit
		case "ctrl+l":
			m.messages = []ChatMessage{}
			m.viewport.SetContent("")
			return m, nil
		case "enter":
			// Shift+Enter / Ctrl+J already handled above via KeyCtrlJ.
			// Here plain Enter submits
			// Use ExpandValue() to replace paste placeholders with actual content.
			displayInput := m.editor.Value()
			input := m.editor.ExpandValue()
			if input == "" {
				return m, nil
			}
			m.recordInputHistory(displayInput)

			if m.clarifyMode {
				response := m.resolveClarifyResponse(input)
				m.addMessage("user", response)
				m.editor.Reset()
				m.clarifyBridge.Submit(m.clarifyReq, response)
				m.clarifyMode = false
				m.clarifyChoices = nil
				m.clarifyReq = tools.ClarifyRequest{}
				m.thinking = true
				m.runStatus = "working"
				m.thinkingStart = time.Now()
				m.thinkingDots = 0
				return m, tea.Batch(m.spinner.Tick, workingTick())
			}

			if strings.HasPrefix(input, "/") {
				return m, m.handleCommand(input)
			}
			m.addMessage("user", input)
			m.editor.Reset()
			m.thinking = true
			m.runStatus = "working"
			m.thinkingStart = time.Now()
			m.thinkingDots = 0
			ctx, cancel := context.WithCancel(context.Background())
			m.cancelFn = cancel
			return m, tea.Batch(m.runAgent(ctx, input), m.spinner.Tick, workingTick())
		}
	}
	m.editor.Update(msg)
	return m, nil
}

func (m *uiModel) resolveClarifyResponse(input string) string {
	cleaned := strings.TrimSpace(input)
	for i, choice := range m.clarifyChoices {
		if cleaned == fmt.Sprintf("%d", i+1) {
			return choice
		}
	}
	return cleaned
}

func (m *uiModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	return m.viewModel()
}

func formatUsage(usage, limit int) string {
	if limit <= 0 {
		return fmt.Sprintf("%s/? tokens", compactCount(usage))
	}
	return fmt.Sprintf("%s/%s tokens", compactCount(usage), compactCount(limit))
}

func resolveUITokenLimit(cfg *config.Config, providerName, modelName string) int {
	if cfg != nil {
		rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{})
		if err == nil && rt.ContextLength > 0 {
			return rt.ContextLength
		}
	}
	return modelruntime.KnownContextLength(providerName, modelName)
}

func compactCount(n int) string {
	if n >= 1_000_000 {
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1000 {
		if n%1000 == 0 {
			return fmt.Sprintf("%dK", n/1000)
		}
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func renderProgressBar(progress float64, width int) string {
	filled := int(progress * float64(width))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if runewidth.StringWidth(stripANSI(line)) <= width {
			result = append(result, line)
			continue
		}
		var cur strings.Builder
		curWidth := 0
		words := strings.Fields(line)
		for _, w := range words {
			wWidth := runewidth.StringWidth(stripANSI(w))
			if curWidth+wWidth+1 <= width {
				if cur.Len() > 0 {
					cur.WriteString(" ")
					curWidth += 1
				}
				cur.WriteString(w)
				curWidth += wWidth
			} else {
				if cur.Len() > 0 {
					result = append(result, cur.String())
				}
				cur.Reset()
				cur.WriteString(w)
				curWidth = wWidth
			}
		}
		if cur.Len() > 0 {
			result = append(result, cur.String())
		}
	}
	return strings.Join(result, "\n")
}

var (
	inlineCodeRegex   = regexp.MustCompile("`.*?`")
	inlineBoldRegex   = regexp.MustCompile(`\*\*.*?\*\*`)
	inlineItalicRegex = regexp.MustCompile(`\*[^* ][^* \n]*\*`)
	inlineLinkRegex   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

func renderMarkdown(s string, width int) string {
	if width < 8 {
		width = 8
	}
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	var result strings.Builder
	lines := strings.Split(s, "\n")
	inCodeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			result.WriteString(codeStyle.Render(line) + "\n")
			continue
		}
		line = inlineCodeRegex.ReplaceAllStringFunc(line, func(match string) string {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render(match[1 : len(match)-1])
		})
		line = inlineBoldRegex.ReplaceAllStringFunc(line, func(match string) string {
			return lipgloss.NewStyle().Bold(true).Render(match[2 : len(match)-2])
		})
		line = inlineItalicRegex.ReplaceAllStringFunc(line, func(match string) string {
			return lipgloss.NewStyle().Italic(true).Render(match[1 : len(match)-1])
		})
		line = inlineLinkRegex.ReplaceAllString(line, "$1 ($2)")
		result.WriteString(wrapText(line, width) + "\n")
	}
	return result.String()
}

type MsgAgentDone struct {
	Response string
	Usage    llm.UsageStats
	Err      error
}

type MsgStream struct {
	Content string
}

type MsgToolStart struct {
	ToolName string
	Args     string
}

type MsgToolOutput struct {
	ToolName string
	Content  string
}

type MsgToolHeartbeat struct {
	ToolName string
	Content  string
}

type MsgToolDone struct {
	ToolName string
	Result   string
	Err      error
	Duration float64
}

type MsgLearningEvent struct {
	Content string
}

type helpRow struct {
	Left  string
	Right string
}

var shortcutHelpRows = []helpRow{
	{Left: "Enter", Right: "Submit the current message"},
	{Left: "Shift+Enter", Right: "Insert a newline"},
	{Left: "Ctrl+J", Right: "Insert a newline"},
	{Left: "Ctrl+C", Right: "Clear input, cancel a running task, close help, or exit"},
	{Left: "Ctrl+L", Right: "Clear the current transcript view"},
	{Left: "Mouse drag", Right: "Use your terminal's native text selection"},
}

var _ tea.Model = (*uiModel)(nil)
