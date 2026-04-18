package cli

import (
	"context"
	"encoding/base64"
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
	sessionSearchFn    func(query string, limit int) (interface{}, error)
	cancelFn           context.CancelFunc
	clarifyMode        bool
	clarifyChoices     []string
	clarifyReq         tools.ClarifyRequest
	secretMode         bool
	secretKey          string
	selectionStart     int // Y coordinate of drag start
	selectionEnd       int // Y coordinate of drag end
	isSelecting        bool
	statusMsg          string    // Transient status message
	thinkingDots       int       // Counter for "..." animation
	thinkingStart      time.Time // When current thinking started
	migrationHint      string    // Hint for migrating Hermes skills
}

type MsgClearStatus struct{}

func NewController(a *kernel.Agent, provider llm.Provider) *Controller {
	c := &common.Common{
		Styles: common.DefaultStyles(),
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	return &Controller{
		model: &uiModel{
			common:       c,
			sidebar:      sidebar.New(c),
			status:       status.New(c),
			editor:       components.NewEditor(c),
			messages:     []ChatMessage{},
			thinking:     false,
			provider:     provider,
			agent:        a,
			tenantID:     "user1",
			channel:      "cli",
			spinner:      sp,
			inputHistory: []string{},
			historyIndex: -1,
			startTime:    time.Now(),
			tokenLimit:   1000000,
			viewport:     viewport.New(0, 0),
		},
	}
}

func NewControllerWithGateway(gw *router.Gateway, agent *kernel.Agent, provider llm.Provider, providerName, modelName string) *Controller {
	c := &common.Common{
		Styles: common.DefaultStyles(),
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	return &Controller{
		model: &uiModel{
			common:       c,
			sidebar:      sidebar.New(c),
			status:       status.New(c),
			editor:       components.NewEditor(c),
			messages:     []ChatMessage{},
			thinking:     false,
			provider:     provider,
			providerName: providerName,
			modelName:    modelName,
			agent:        agent,
			gateway:      gw,
			tenantID:     "user1",
			channel:      "cli",
			spinner:      sp,
			inputHistory: []string{},
			historyIndex: -1,
			startTime:    time.Now(),
			tokenLimit:   1000000,
			viewport:     viewport.New(0, 0),
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

func (c *Controller) checkMigration() {
	if !app.NeedsMigration() {
		return
	}

	c.model.migrationHint = "Type /migrate to import your Hermes skills."
}

func (c *Controller) Start() {
	c.checkMigration()
	tools.RegisterClarifyCallback()
	p := tea.NewProgram(c.model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	c.model.program = p
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
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

func (m *uiModel) viewModel() string {
	mainW := m.width
	st := m.common.Styles

	headerText := st.Header.Title.Render(" SelfMind ") +
		st.Header.Separator.Render("│") + " " +
		st.Header.Subtitle.Render("Agent Core")
	header := lipgloss.NewStyle().Width(m.width).Render(headerText)

	fullContent := m.renderAllMessages()

	// ── Input area ───────────────────────────────────────────────────
	suggestion := m.editor.GetSuggestion()
	inputH := 3
	if suggestion != "" {
		inputH = 4
	}
	inputRect := layout.Rect{W: m.width, H: inputH}
	inputArea := m.editor.Draw(inputRect)

	// Adjust viewport height: height - header(1) - inputArea(inputH) - status(1)
	visibleH := m.height - inputH - 2
	if m.migrationHint != "" {
		visibleH--
	}
	if visibleH < 1 {
		visibleH = 1
	}

	m.viewport.Width = mainW
	m.viewport.Height = visibleH
	m.viewport.SetContent(fullContent)

	mainStr := st.Main.Width(mainW).Height(visibleH).Render(m.viewport.View())

	// Transient status/notification area
	var notification string
	if m.statusMsg != "" {
		notification = lipgloss.NewStyle().
			Foreground(lipgloss.Color("203")).
			Italic(true).
			PaddingLeft(2).
			Render("! " + m.statusMsg)
	}

	// Migration Hint area
	var migrationHint string
	if m.migrationHint != "" {
		migrationHint = lipgloss.NewStyle().
			Foreground(lipgloss.Color("212")).
			Italic(true).
			PaddingLeft(2).
			Render("✨ " + m.migrationHint)
	}

	statusBar := st.Status.Panel.Width(m.width).Render(m.statusLine())

	return lipgloss.JoinVertical(lipgloss.Left, header, mainStr, notification, migrationHint, inputArea, statusBar)
}

func (m *uiModel) statusLine() string {
	st := m.common.Styles
	modelName := m.modelName
	if modelName == "" {
		modelName = m.providerName
	}
	if modelName == "" {
		modelName = "active"
	}
	header := modelName

	usageStr := formatUsage(m.totalTokens, m.tokenLimit)

	elapsed := time.Since(m.startTime)
	timeStr := formatDuration(elapsed)

	parts := []string{
		st.Status.Value.Render(header),
		st.Status.Label.Render(usageStr),
		st.Status.Value.Render(timeStr),
	}

	if m.thinking {
		dots := strings.Repeat(".", (m.thinkingDots%3)+1)
		elapsed := time.Since(m.thinkingStart).Seconds()
		parts = append(parts, st.Status.Good.Render(fmt.Sprintf("Thinking%s (%.1fs)", dots, elapsed)))
	}
	if m.toolExecuting != "" {
		parts = append(parts, st.Status.Warning.Render("⚙ "+m.toolExecuting))
	}

	return strings.Join(parts, "  •  ")
}

func (m *uiModel) renderAllMessages() string {
	st := m.common.Styles
	w := m.viewport.Width
	if w <= 0 {
		w = 60
	}

	var allLines []string

	// Calculate selection range
	startY, endY := m.selectionStart, m.selectionEnd
	if startY > endY {
		startY, endY = endY, startY
	}
	viewportTop := 1
	scrollOffset := m.viewport.YOffset
	lineStart := scrollOffset + (startY - viewportTop)
	lineEnd := scrollOffset + (endY - viewportTop)

	processLines := func(lines []string, baseIdx int) []string {
		if !m.isSelecting {
			return lines
		}
		for i := range lines {
			globalLineIdx := baseIdx + i
			if globalLineIdx >= lineStart && globalLineIdx <= lineEnd {
				plain := stripANSI(lines[i])
				// Hermes-style: ensure the line fills the viewport width with the selection background
				display := plain
				if display == "" {
					display = " "
				}
				lines[i] = st.Chat.Selected.Copy().Width(w).Render(display)
			}
		}
		return lines
	}

	if len(m.messages) == 0 && !m.thinking {
		welcomeLines := strings.Split(st.Welcome, "\n")
		welcomeLines = processLines(welcomeLines, 0)
		allLines = append(allLines, welcomeLines...)
	}

	for _, msg := range m.messages {
		var rendered string
		switch msg.Role {
		case "user":
			header := st.Chat.UserBubble.Render("You")
			body := st.Chat.UserText.Width(w).Render(stripANSI(msg.Content))
			rendered = header + "\n" + body + "\n"
		case "assistant":
			header := st.Chat.AssistantBubble.Render("SelfMind")
			body := renderMarkdown(stripANSI(msg.Content), w)
			rendered = header + "\n" + body + "\n"
		case "tool":
			label := msg.ToolName
			if label == "" {
				label = "tool"
			}
			
			// Icon and Action mapping
			emoji := "⚡"
			action := label
			detail := ""
			
			// Parse JSON args for human readable display
			var args map[string]interface{}
			json.Unmarshal([]byte(msg.ToolArgs), &args)
			
			switch label {
			case "ls_r", "list_files", "search_files", "grep":
				emoji = "🔍"
				action = "search"
				if v, ok := args["path"].(string); ok { detail = v }
				if v, ok := args["pattern"].(string); ok { detail = v }
			case "cat", "read_file":
				emoji = "📖"
				action = "read"
				if v, ok := args["path"].(string); ok { detail = v }
			case "terminal", "execute_command", "shell":
				emoji = "💻"
				action = "$"
				if v, ok := args["command"].(string); ok { detail = v }
			case "patch":
				emoji = "🔧"
				action = "patch"
				if v, ok := args["path"].(string); ok { detail = v }
			case "write_file":
				emoji = "✍️"
				action = "write"
				if v, ok := args["path"].(string); ok { detail = v }
			}

			prefix := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("  ┊ ")

			if msg.Content == "" {
				rendered = fmt.Sprintf("%s%s %-9s preparing %s…\n", prefix, emoji, "", label)
			} else {
				dur := fmt.Sprintf("%.1fs", msg.Duration)
				if msg.Duration == 0 {
					dur = "0.1s"
				}

				if detail == "" {
					detail = "..."
				}

				errorSuffix := ""
				if msg.IsError {
					errorSuffix = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render(" [error]")
				}

				mainLine := fmt.Sprintf("%s%s %-9s %s  %s%s\n", prefix, emoji, action, detail, dur, errorSuffix)
				rendered = mainLine
			}
		case "system":
			rendered = st.Chat.ToolBubble.BorderForeground(lipgloss.Color("203")).Width(w - 2).Render("! SYSTEM: "+stripANSI(msg.Content)) + "\n"
		}

		msgLines := strings.Split(rendered, "\n")
		msgLines = processLines(msgLines, len(allLines))
		allLines = append(allLines, msgLines...)
	}

	if m.thinking {
		spinnerView := m.spinner.View()
		dots := strings.Repeat(".", (m.thinkingDots%3)+1)
		rendered := st.Chat.Thinking.Render(spinnerView + " Thinking" + dots)
		lines := processLines([]string{rendered}, len(allLines))
		allLines = append(allLines, lines...)
	}

	if m.toolExecuting != "" {
		rendered := st.Chat.ToolBubble.Width(w - 2).Render("⚙ Executing: " + m.toolExecuting)
		lines := processLines([]string{rendered}, len(allLines))
		allLines = append(allLines, lines...)
	}

	// Pad with empty selectable lines up to viewport height if needed
	minLines := m.viewport.Height + m.viewport.YOffset
	for len(allLines) < minLines {
		line := ""
		if m.isSelecting {
			idx := len(allLines)
			if idx >= lineStart && idx <= lineEnd {
				line = st.Chat.Selected.Copy().Width(w).Render(" ")
			}
		}
		allLines = append(allLines, line)
	}

	return strings.Join(allLines, "\n")
}

func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if tools.ClarifyEventChan != nil {
		select {
		case req := <-tools.ClarifyEventChan:
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
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		if msg.Button == tea.MouseButtonLeft {
			if msg.Action == tea.MouseActionPress {
				m.isSelecting = true
				m.selectionStart = msg.Y
				m.selectionEnd = msg.Y
			} else if msg.Action == tea.MouseActionRelease {
				if m.isSelecting {
					m.selectionEnd = msg.Y
					m.isSelecting = false
					return m, m.copySelection()
				}
			} else {
				m.selectionEnd = msg.Y
			}
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
			m.addMessage("assistant", fmt.Sprintf("Error: %v", msg.Err))
		} else {
			m.addMessage("assistant", msg.Response)
		}
		return m, spinnerCmd

	case MsgToolStart:
		m.toolExecuting = msg.ToolName
		m.addMessage("tool", "")
		last := &m.messages[len(m.messages)-1]
		last.ToolName = msg.ToolName
		last.ToolArgs = msg.Args
		return m, spinnerCmd

	case MsgToolDone:
		m.toolExecuting = ""
		if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == "tool" {
			last := &m.messages[len(m.messages)-1]
			last.ToolName = msg.ToolName
			last.Duration = msg.Duration
			if msg.Err != nil {
				last.Content = fmt.Sprintf("%s error: %v", msg.ToolName, msg.Err)
				last.IsError = true
			} else {
				last.Content = msg.Result
				last.IsError = false
			}
		}
		return m, spinnerCmd

	case MsgClearStatus:
		m.statusMsg = ""
		return m, nil

	default:
		cmd := m.editor.Update(msg)
		return m, cmd
	}
}

func (m *uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.thinking || m.toolExecuting != "" {
			if m.cancelFn != nil {
				m.cancelFn()
				m.thinking = false
				m.toolExecuting = ""
				m.statusMsg = "Task cancelled by user."
				return m, tea.Tick(time.Second*3, func(t time.Time) tea.Msg {
					return MsgClearStatus{}
				})
			}
			return m, nil
		}
		return m, tea.Quit
	case "ctrl+l":
		m.messages = []ChatMessage{}
		m.viewport.SetContent("")
		return m, nil
	case "enter":
		input := m.editor.Value()
		if input == "" {
			return m, nil
		}

		if strings.HasPrefix(input, "/") {
			return m, m.handleCommand(input)
		}
		m.addMessage("user", input)
		m.thinking = true
		m.thinkingStart = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		m.cancelFn = cancel
		// FIXED: Return spinner.Tick to ensure the animation starts immediately
		return m, tea.Batch(m.runAgent(ctx, input), m.spinner.Tick)
	}
	m.editor.Update(msg)
	return m, nil
}

func (m *uiModel) handleCommand(input string) tea.Cmd {
	parts := strings.Fields(input)
	cmd := parts[0]
	switch cmd {
	case "/help":
		m.addMessage("assistant", helpText)
	case "/clear":
		m.messages = []ChatMessage{}
		m.viewport.SetContent("")
	case "/exit":
		return tea.Quit
	case "/migrate":
		return m.handleMigration()
	case "/status":
		return m.handleStatus()
	case "/tasks":
		return m.handleTasks()
	case "/checkpoint":
		if len(parts) < 2 {
			m.addMessage("assistant", "Usage: /checkpoint [list|save|load] [name]")
			return nil
		}
		return m.handleCheckpoint(parts[1:])
	}
	return nil
}

func (m *uiModel) handleMigration() tea.Cmd {
	return func() tea.Msg {
		dir, exists := app.CheckHermesSkills()
		if !exists {
			return MsgAgentDone{Response: "No Hermes skills found to migrate."}
		}
		count, err := app.MigrateHermesSkills(dir)
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Migration error: %v", err)}
		}
		m.migrationHint = "" // Clear hint after success
		return MsgAgentDone{Response: fmt.Sprintf("Successfully migrated %d skills from Hermes!", count)}
	}
}

func (m *uiModel) handleStatus() tea.Cmd {
	return func() tea.Msg {
		elapsed := time.Since(m.startTime)
		usage := formatUsage(m.totalTokens, m.tokenLimit)
		
		status := fmt.Sprintf("## System Status\n\n- **Provider**: %s\n- **Model**: %s\n- **Uptime**: %s\n- **Token Usage**: %s\n", 
			m.providerName, m.modelName, formatDuration(elapsed), usage)

		// Current task
		if m.gateway != nil {
			t, err := m.gateway.GetCurrentTaskInfo(context.Background(), m.tenantID)
			if err == nil && t != nil {
				status += fmt.Sprintf("- **Current Task**: [%d] %s\n", t.ID, t.Title)
			} else {
				status += "- **Current Task**: None\n"
			}
		}

		// Background processes
		registry := tools.GetProcessRegistry()
		procs := registry.List()
		if len(procs) > 0 {
			status += "\n### Background Processes\n"
			for _, p := range procs {
				idStr := p["id"].(string)
				if len(idStr) > 8 { idStr = idStr[:8] }
				status += fmt.Sprintf("- `%s`: %s (%s)\n", idStr, p["command"], p["status"])
			}
		}

		return MsgAgentDone{Response: status}
	}
}

func (m *uiModel) handleTasks() tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return MsgAgentDone{Response: "Gateway not initialized, cannot list tasks."}
		}
		tasks, err := m.gateway.ListTasks(context.Background(), m.tenantID)
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Error fetching tasks: %v", err)}
		}
		if len(tasks) == 0 {
			return MsgAgentDone{Response: "No tasks found."}
		}
		var sb strings.Builder
		sb.WriteString("## Global Tasks\n\n")
		for _, t := range tasks {
			status := "⏳"
			if t.Status == "done" { status = "✅" }
			if t.Status == "cancelled" { status = "❌" }
			sb.WriteString(fmt.Sprintf("%s [%d] %s (Created: %s)\n", 
				status, t.ID, t.Title, t.CreatedAt.Format("01-02 15:04")))
		}
		return MsgAgentDone{Response: sb.String()}
	}
}

func (m *uiModel) handleCheckpoint(args []string) tea.Cmd {
	action := args[0]
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	return func() tea.Msg {
		resp, err := m.agent.Dispatcher().Dispatch("checkpoint", map[string]interface{}{
			"action": action,
			"name":   name,
		})
		if err != nil {
			return MsgAgentDone{Response: fmt.Sprintf("Checkpoint error: %v", err)}
		}
		return MsgAgentDone{Response: resp}
	}
}

func (m *uiModel) copySelection() tea.Cmd {
	start := m.selectionStart
	end := m.selectionEnd
	if start > end {
		start, end = end, start
	}
	// Viewport offset adjustment: header(1)
	viewportTop := 1
	fullLines := m.renderAllMessagesLines()
	scrollOffset := m.viewport.YOffset
	lineStart := scrollOffset + (start - viewportTop)
	lineEnd := scrollOffset + (end - viewportTop)
	if lineStart < 0 {
		lineStart = 0
	}
	if lineEnd >= len(fullLines) {
		lineEnd = len(fullLines) - 1
	}
	if lineStart > lineEnd {
		return nil
	}
	selectedText := ""
	var cleanLines []string
	for _, line := range fullLines[lineStart : lineEnd+1] {
		// Trim only trailing UI padding spaces, preserve leading indentation
		clean := strings.TrimRight(stripANSI(line), " ")
		cleanLines = append(cleanLines, clean)
	}
	selectedText = strings.Join(cleanLines, "\n")

	if selectedText == "" {
		return nil
	}

	return func() tea.Msg {
		// OSC 52 Copy to Clipboard
		b64 := base64.StdEncoding.EncodeToString([]byte(selectedText))
		fmt.Printf("\x1b]52;c;%s\a", b64)

		m.statusMsg = "Selected text copied to clipboard!"
		go func() {
			time.Sleep(2 * time.Second)
			if m.program != nil {
				m.program.Send(MsgClearStatus{})
			}
		}()
		return nil
	}
}

func (m *uiModel) renderAllMessagesLines() []string {
	content := m.renderAllMessages()
	return strings.Split(content, "\n")
}

func (m *uiModel) runAgent(ctx context.Context, input string) tea.Cmd {
	return func() tea.Msg {
		go m.pumpAgentEvents()
		
		// Use Gateway instead of calling Agent directly
		if m.gateway != nil {
			resp, err := m.gateway.Handle(ctx, m.tenantID, m.channel, input)
			if err != nil {
				return MsgAgentDone{Err: err}
			}
			
			if !resp.IsStreaming {
				return MsgAgentDone{Response: resp.Content, Usage: resp.Usage, Err: nil}
			}

			// For streaming, we wait for the stream to close in pumpAgentEvents 
			// or handle it here. Since gateway.Handle already launched a goroutine 
			// that calls agent.RunConversation, and agent.RunConversation emits 
			// events to EventChannel, pumpAgentEvents will catch them.
			// We just need to wait for the final completion event from the stream.
			for event := range resp.Stream {
				if event.Err != nil {
					return MsgAgentDone{Err: event.Err}
				}
				if event.Usage != nil {
					// Final usage stats
					return MsgAgentDone{Usage: *event.Usage}
				}
			}
			return nil // Result already sent via events
		}

		// Fallback for direct agent access (backward compatibility)
		resp, usage, err := m.agent.RunConversation(ctx, m.tenantID, m.channel, input)
		return MsgAgentDone{Response: resp, Usage: usage, Err: err}
	}
}

func (m *uiModel) pumpAgentEvents() {
	if m.agent == nil || m.agent.EventChannel == nil {
		return
	}
	for {
		select {
		case event, ok := <-m.agent.EventChannel:
			if !ok {
				return
			}
			switch {
			case strings.HasPrefix(event, "stream:"):
				content := strings.TrimPrefix(event, "stream:")
				if m.program != nil {
					m.program.Send(MsgStream{Content: content})
				}
			case strings.HasPrefix(event, "tool_start:"):
				parts := strings.SplitN(event[11:], ":", 2)
				name := parts[0]
				args := ""
				if len(parts) > 1 {
					args = parts[1]
				}
				if m.program != nil {
					m.program.Send(MsgToolStart{ToolName: name, Args: args})
				}
			case strings.HasPrefix(event, "tool_end:"):
				rest := strings.TrimPrefix(event, "tool_end:")
				parts := strings.SplitN(rest, ":", 3)
				name := parts[0]
				durationStr := "0"
				result := ""
				var err error
				if len(parts) >= 2 {
					if parts[1] == "error" {
						errParts := strings.SplitN(parts[2], ":", 2)
						durationStr = errParts[0]
						err = fmt.Errorf("%s", errParts[1])
					} else {
						durationStr = parts[1]
						result = parts[2]
					}
				}
				var duration float64
				fmt.Sscanf(durationStr, "%f", &duration)
				if m.program != nil {
					m.program.Send(MsgToolDone{ToolName: name, Result: result, Err: err, Duration: duration})
				}
			}
		}
	}
}

func (m *uiModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	return m.viewModel()
}

func formatUsage(usage, limit int) string {
	u := float64(usage) / 1000.0
	l := float64(limit) / 1000.0
	return fmt.Sprintf("%.0fK/%.0fK", u, l)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
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
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Background(lipgloss.Color("235"))
	var result strings.Builder
	lines := strings.Split(s, "\n")
	inCodeBlock := false
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			result.WriteString(codeStyle.Render("```\n"))
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			result.WriteString(codeStyle.Render("  " + line + "\n"))
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

type MsgToolDone struct {
	ToolName string
	Result   string
	Err      error
	Duration float64
}

const helpText = `Available commands:
  /help       - Show this help
  /clear      - Clear conversation history
  /status     - Show system status and background processes
  /tasks      - List global tasks
  /checkpoint - Conversation snapshot (list|save|load)
  /migrate    - Migrate skills from Hermes Agent
  /exit       - Exit (or Ctrl+C)
  
Shortcuts:
  Ctrl+C      - Cancel running task or Exit
  Ctrl+L      - Clear screen`

var _ tea.Model = (*uiModel)(nil)
