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
	"github.com/google/uuid"
	"github.com/mattn/go-runewidth"
	"selfmind/internal/app"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/textutil"
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

type MessageProcessor func(context.Context, api.MessageRequest) (api.MessageResponse, int)

// ChatMessage represents a single message in the conversation history.
type ChatMessage struct {
	Role          string // "user", "assistant", "system", "tool"
	Content       string
	Timestamp     time.Time
	ToolName      string // populated when Role == "tool"
	ToolCallID    string
	ToolArgs      string  // Fix: add ToolArgs to store call arguments
	Duration      float64 // Fix: add Duration for performance display
	IsError       bool    // Fix: add IsError flag
	IsRunning     bool
	RunningDetail string
	// Committed is set in terminal-first hybrid mode once this message has been
	// printed into native scrollback. Committed messages are immutable and are
	// not re-rendered in the active region. Unused in legacy (viewport) mode.
	Committed bool
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
	runTokens          int
	totalTokens        int
	tokenLimit         int
	modelMeta          string
	startTime          time.Time
	provider           llm.Provider
	providerName       string
	modelName          string
	agent              *kernel.Agent
	gateway            *router.Gateway
	messageProcessor   MessageProcessor
	tenantID           string
	channel            string // 'cli' | 'wechat' | 'dingtalk' | 'web'
	approvalMode       string // codex-style: on-request | read-only | auto-edit | full-auto
	spinner            spinner.Model
	inputHistory       []string
	historyIndex       int
	historyDraft       string
	sessionSearchFn    func(query string, limit int) (interface{}, error)
	clarifyBridge      *tools.ClarifyBridge
	cancelFn           context.CancelFunc
	steerCh            chan string // mid-turn guidance channel for the active run (nil when idle)
	clarifyMode        bool
	clarifyChoices     []string
	clarifyReq         tools.ClarifyRequest
	secretMode         bool
	secretKey          string
	statusMsg          string    // Transient status message
	thinkingDots       int       // Counter for "..." animation
	thinkingStart      time.Time // When current thinking started
	activityText       string    // Current model/tool phase shown in transcript
	runStatus          string    // ready | working | done | error | cancelled
	migrationHint      string    // Hint for migrating Hermes skills
	streamController   markdownStreamController
	liveStreamContent  string
	streamFlushPending bool
	cursorVisible      bool
	mouseDragActive    bool
	mouseAutoScrollDir int
	mouseScrollTicking bool
	mouseSelection     bool
	mouseSelectAnchor  int
	mouseSelectFocus   int
	transcriptCache    *renderCache // memoizes finalized message renders across frames
	clientMode         bool         // daemon-client mode: no in-process agent/gateway; chat routes to the daemon
	toolDispatchFn     func(tool string, args map[string]interface{}) (string, error) // client mode: run management tools on the daemon
	approvalResponder  func(approvalID, decision, scope string) error // client mode: answer a daemon tool-approval request (scope: ""|task|person)
	steerFn            func(text string) error                        // client mode: forward mid-turn guidance to the daemon's active run
	// Interactive approval panel state (see approval_flow.go). approvalPrompt is
	// the active panel; approvalQueue holds requests that arrived while one was
	// already up (FIFO re-arm); approvalDenyFollowup captures the composer after
	// "No" so the user can attach guidance to the rejection.
	approvalPrompt       *components.ApprovalPrompt
	approvalQueue        []MsgApprovalRequest
	approvalDenyFollowup bool
	pendingApprovalID    string
	pendingApprovalTool  string
	// Attach digest + re-attach (client mode, G0-c/G0-d): the client shell
	// fetches the digest before the first presence beat and hands it over via
	// SetStartupDigest; the first sized frame renders it once, and when it
	// reports a mid-flight run the runWatcher hooks its live events.
	startupDigest *api.DigestResponse
	digestShown   bool
	runWatcher    RunWatcher
	// watchingRun is PASSIVE spectator mode for a pre-existing daemon run
	// (no local turn owns it, the composer is NOT captured). Distinct from
	// m.thinking, which means "a local turn is executing".
	watchingRun      bool
	watchedTaskTitle string
	watchCancel      context.CancelFunc
	// exitPromptActive intercepts keys while the quit-with-active-run prompt
	// is shown (b = background+quit, c = cancel+stay, esc = keep watching).
	exitPromptActive bool
	hybrid             bool         // terminal-first hybrid mode (SELFMIND_TUI_HYBRID)
	pendingPrintln     []string     // hybrid: cells to emit to scrollback at end of Update
	startupCommitted   bool         // hybrid: startup card already printed to scrollback
}

type MsgClearStatus struct{}
type MsgWorkingTick time.Time
type MsgCursorBlinkTick time.Time
type MsgMouseAutoScrollTick time.Time
type MsgStreamFlush time.Time
type MsgAgentActivity struct {
	Content string
}

// MsgApprovalRequest is emitted (client mode) when the daemon blocks a run
// waiting for tool approval. The TUI renders the interactive approval panel in
// the active region and answers via the approval responder.
type MsgApprovalRequest struct {
	ID     string
	Tool   string
	Target string // compact object of the action (path/command); may be empty
	Reason string
}

func workingTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return MsgWorkingTick(t)
	})
}

func cursorBlinkTick() tea.Cmd {
	return tea.Tick(530*time.Millisecond, func(t time.Time) tea.Msg {
		return MsgCursorBlinkTick(t)
	})
}

func streamFlushTick() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(t time.Time) tea.Msg {
		return MsgStreamFlush(t)
	})
}

func mouseAutoScrollTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg {
		return MsgMouseAutoScrollTick(t)
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
			cursorVisible: true,
			provider:      provider,
			agent:         a,
			tenantID:      tenantID,
			channel:       cliSessionChannel(),
			spinner:       sp,
			inputHistory:  []string{},
			historyIndex:  -1,
			approvalMode:  "on-request",
			startTime:     time.Now(),
			runStatus:     "ready",
			tokenLimit:    resolveUITokenLimit(cfg, "", ""),
			modelMeta:     resolveUIModelMeta(cfg),
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
			cursorVisible: true,
			provider:      provider,
			providerName:  providerName,
			modelName:     modelName,
			agent:         agent,
			gateway:       gw,
			tenantID:      tenantID,
			channel:       cliSessionChannel(),
			spinner:       sp,
			inputHistory:  []string{},
			historyIndex:  -1,
			approvalMode:  "on-request",
			startTime:     time.Now(),
			runStatus:     "ready",
			tokenLimit:    resolveUITokenLimit(cfg, providerName, modelName),
			modelMeta:     resolveUIModelMeta(cfg),
			viewport:      viewport.New(0, 0),
			clarifyBridge: tools.NewClarifyBridge(),
		},
	}
}

func (c *Controller) SetSessionSearchFn(fn func(query string, limit int) (interface{}, error)) {
	c.model.sessionSearchFn = fn
}

func (c *Controller) SetMessageProcessor(processor MessageProcessor) {
	if c == nil || c.model == nil {
		return
	}
	c.model.messageProcessor = processor
}

// SetClientMode marks the controller as a thin client to a gateway daemon. In
// this mode there is no in-process agent/gateway, so chat routes through the
// message processor and agent-backed slash commands route through the tool
// dispatch function (or degrade to a clear notice) instead of dereferencing a
// nil agent. See docs/worker-pool-design.md.
func (c *Controller) SetClientMode(enabled bool) {
	if c == nil || c.model == nil {
		return
	}
	c.model.clientMode = enabled
}

// SetApprovalResponder installs the function used to answer a daemon
// tool-approval request from the client TUI's approval panel. scope carries the
// class-grant memory ("" once, "task", "person") through the existing
// /v1/approvals/respond path. Only set in client mode; in-process approvals use
// the clarify bridge instead.
func (c *Controller) SetApprovalResponder(fn func(approvalID, decision, scope string) error) {
	if c == nil || c.model == nil {
		return
	}
	c.model.approvalResponder = fn
}

// SetSteerFunc installs the client-mode forwarder for mid-turn guidance
// (gateway POST /v1/runs/steer). When the run executes inside the daemon, the
// process-local steering channel can never reach it, so input typed during a
// run must go through this function instead. Only set in client mode.
func (c *Controller) SetSteerFunc(fn func(text string) error) {
	if c == nil || c.model == nil {
		return
	}
	c.model.steerFn = fn
}

// SetToolDispatch installs the daemon-backed dispatcher used by agent-backed
// slash commands (/skills, /memory subcommands, /bundles, /curator,
// /checkpoint) in client mode. When set, uiModel.dispatch routes through it
// instead of the in-process agent backend.
func (c *Controller) SetToolDispatch(fn func(tool string, args map[string]interface{}) (string, error)) {
	if c == nil || c.model == nil {
		return
	}
	c.model.toolDispatchFn = fn
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

// SessionChannel returns the chat channel this session ran on. Chats are
// channel-local (per AGENTS.md), so reusing this value via `selfmind --resume
// <uuid>` resumes the same conversation history. Used by the CLI to print a
// resume hint on exit.
func (c *Controller) SessionChannel() string {
	if c == nil || c.model == nil {
		return ""
	}
	return c.model.channel
}

// SetSessionChannel overrides the chat channel for this session. The CLI calls
// it when the user resumes a prior session with `selfmind --resume <uuid>`, so
// this process converges on that session's channel-local history. Must be
// called before Start(); channel is only read at message-send time.
func (c *Controller) SetSessionChannel(ch string) {
	ch = strings.TrimSpace(ch)
	if c == nil || c.model == nil || ch == "" {
		return
	}
	c.model.channel = ch
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
	c.model.hybrid = hybridMode()
	// Terminal-first hybrid is the default: run inline (no alt-screen, so
	// finalized cells can be committed to native scrollback) and without mouse
	// capture (so the terminal owns selection/scroll). SELFMIND_TUI_LEGACY=1
	// falls back to the alt-screen viewport with app-owned mouse handling.
	var opts []tea.ProgramOption
	if !c.model.hybrid {
		opts = append(opts, tea.WithAltScreen(), tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(c.model, opts...)
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

// cliSessionChannelID is unique per CLI process so two terminals run by the
// same user don't share a channel-local conversation (chats are channel-local
// per AGENTS.md; a shared "cli" channel made concurrent sessions bleed context).
// It doubles as the resumable session id printed on exit and accepted by
// `selfmind --resume <uuid>`, so it must be a stable, opaque identifier.
var cliSessionChannelID = uuid.NewString()

// cliSessionChannel returns this process's chat channel. SELFMIND_CHANNEL
// overrides it with a stable, explicitly-shared channel when the user wants
// resumable/shared sessions across terminals.
func cliSessionChannel() string {
	if v := strings.TrimSpace(os.Getenv("SELFMIND_CHANNEL")); v != "" {
		return v
	}
	return cliSessionChannelID
}

func (m *uiModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, cursorBlinkTick())
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return textutil.CleanUTF8(ansiRegex.ReplaceAllString(s, ""))
}

func (m *uiModel) addMessage(role, content string) {
	content = textutil.CleanUTF8(content)
	m.messages = append(m.messages, ChatMessage{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	})
	// In hybrid mode a non-tool message is final on arrival, so commit it to
	// scrollback now. Tool messages start as "running" and are committed later
	// by MsgToolDone.
	if role != "tool" {
		m.commit(&m.messages[len(m.messages)-1])
	}
	m.trimHistoryWindow()
	m.viewport.GotoBottom()
}

func (m *uiModel) addErrorMessage(content string) {
	content = textutil.CleanUTF8(content)
	m.messages = append(m.messages, ChatMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now(),
		IsError:   true,
	})
	m.commit(&m.messages[len(m.messages)-1])
	m.viewport.GotoBottom()
}

func (m *uiModel) appendToolOutput(toolName, content string) {
	content = textutil.CleanUTF8(content)
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

func (m *uiModel) findToolMessageIndex(toolCallID, toolName string) int {
	if toolCallID != "" {
		for i := len(m.messages) - 1; i >= 0; i-- {
			msg := m.messages[i]
			if msg.Role == "tool" && msg.ToolCallID == toolCallID {
				return i
			}
		}
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Role != "tool" || !msg.IsRunning {
			continue
		}
		if toolName == "" || msg.ToolName == "" || msg.ToolName == toolName {
			return i
		}
	}
	if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == "tool" {
		return len(m.messages) - 1
	}
	return -1
}

func (m *uiModel) anyToolRunning() bool {
	for i := range m.messages {
		if m.messages[i].Role == "tool" && m.messages[i].IsRunning {
			return true
		}
	}
	return false
}

func isGenericToolHeartbeat(toolName, content string) bool {
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	content = strings.ToLower(strings.TrimSpace(content))
	if content == "" {
		return true
	}
	if content == "tool running" || content == "running" {
		return true
	}
	if toolName != "" && (content == toolName+" running" || content == "running "+toolName) {
		return true
	}
	return false
}

func (m *uiModel) appendAssistantResponse(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if len(m.messages) > 0 {
		last := &m.messages[len(m.messages)-1]
		// Never merge into a committed cell — in hybrid mode it already lives in
		// immutable scrollback and cannot be rewritten.
		if last.Role == "assistant" && !last.IsError && !last.Committed {
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

func (m *uiModel) commitLiveStream(content string) bool {
	content = textutil.CleanUTF8(content)
	if strings.TrimSpace(content) == "" {
		return false
	}
	m.liveStreamContent += content
	return true
}

func (m *uiModel) flushLiveStreamPending() bool {
	return m.commitLiveStream(m.streamController.Flush())
}

func (m *uiModel) clearLiveStream() {
	m.streamController.Reset()
	m.liveStreamContent = ""
	m.streamFlushPending = false
}

func (m *uiModel) finalizeLiveStream(finalContent string) bool {
	m.flushLiveStreamPending()
	live := strings.TrimSpace(m.liveStreamContent)
	finalContent = strings.TrimSpace(textutil.CleanUTF8(finalContent))
	content := finalContent
	if content == "" {
		content = live
	}
	m.clearLiveStream()
	if content == "" {
		return false
	}
	m.appendAssistantResponse(content)
	return true
}

func (m *uiModel) scheduleStreamFlush() tea.Cmd {
	if m.streamFlushPending || !m.streamController.Pending() {
		return nil
	}
	m.streamFlushPending = true
	return streamFlushTick()
}

func (m *uiModel) transcriptAtBottom() bool {
	return m.viewport.AtBottom() || m.viewport.PastBottom()
}

func (m *uiModel) restoreOrFollowTranscript(wasAtBottom bool, yOffset int) {
	m.syncTranscriptContent()
	if wasAtBottom {
		m.viewport.GotoBottom()
		return
	}
	m.viewport.SetYOffset(yOffset)
}

func (m *uiModel) transcriptVisibleHeight() int {
	if m == nil {
		return 1
	}
	inputH := 1
	if m.editor != nil {
		inputH = m.editor.PreferredHeight()
	}
	visibleH := m.height - inputH - 1 - m.composerGapHeight()
	if m.notificationBar(m.width) != "" {
		visibleH--
	}
	if m.migrationHintBar(m.width) != "" {
		visibleH--
	}
	// Legacy mode draws the approval panel between transcript and composer, so
	// the viewport gives up that many rows.
	if m.approvalPrompt != nil {
		visibleH -= lipgloss.Height(m.approvalPrompt.View(m.width))
	}
	if visibleH < 1 {
		visibleH = 1
	}
	return visibleH
}

func (m *uiModel) composerGapHeight() int {
	if m == nil || m.height < 12 {
		return 0
	}
	inputH := 1
	if m.editor != nil {
		inputH = m.editor.PreferredHeight()
	}
	occupied := inputH + 1 // input area + status bar
	if m.notificationBar(m.width) != "" {
		occupied++
	}
	if m.migrationHintBar(m.width) != "" {
		occupied++
	}
	if m.height-occupied <= 6 {
		return 0
	}
	return 1
}

func (m *uiModel) mouseInTranscript(msg tea.MouseMsg) bool {
	return msg.Y >= 0 && msg.Y < m.transcriptVisibleHeight()
}

func (m *uiModel) viewModel() string {
	if m.hybrid {
		return m.viewActiveRegion()
	}
	if m.pager != nil {
		return m.pager.View()
	}

	mainW := m.width
	st := m.common.Styles

	wasAtBottom := m.viewport.AtBottom() || m.viewport.PastBottom()

	m.editor.SetCursorVisible(m.cursorVisible)
	inputH := m.editor.PreferredHeight()
	inputRect := layout.Rect{W: m.width, H: inputH}
	inputArea := m.editor.Draw(inputRect)

	notification := m.notificationBar(mainW)
	migrationHint := m.migrationHintBar(mainW)

	// Codex-cli style: transcript + composer + one-line footer.
	visibleH := m.transcriptVisibleHeight()

	m.viewport.Width = mainW
	m.viewport.Height = visibleH
	fullContent := m.renderAllMessages()
	stickToBottom := wasAtBottom
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

	parts := []string{mainStr}
	// The approval panel is active-region content in BOTH renderers: in legacy
	// mode it sits between the transcript viewport and the composer.
	if m.approvalPrompt != nil {
		parts = append(parts, m.approvalPrompt.View(mainW))
	}
	if notification != "" {
		parts = append(parts, notification)
	}
	if migrationHint != "" {
		parts = append(parts, migrationHint)
	}
	if gapH := m.composerGapHeight(); gapH > 0 {
		parts = append(parts, lipgloss.NewStyle().Width(m.width).Height(gapH).Render(""))
	}
	if hint := m.composerHint(); hint != "" {
		parts = append(parts, hint)
	}
	parts = append(parts, inputArea, statusBar)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// notificationBar renders a transient status notice (clipboard, mid-turn
// steering, cancellation) as a compact, left-aligned colored line with a
// leading glyph rather than a full-width grey slab. The slab read as leftover
// terminal output; a categorized accent line reads as a deliberate notice and
// makes the message kind (success / info / warning) obvious at a glance.
func (m *uiModel) notificationBar(width int) string {
	if width <= 0 || strings.TrimSpace(m.statusMsg) == "" {
		return ""
	}
	text := strings.TrimSpace(m.statusMsg)
	glyph, color := notificationStyleFor(text)
	body := glyph + " " + text
	return lipgloss.NewStyle().
		Padding(0, 1).
		Foreground(lipgloss.Color(color)).
		Bold(true).
		Render(truncateToWidth(body, width-2))
}

// notificationStyleFor classifies a transient status message into a glyph and
// accent color. It keys off the stable phrases the controller emits (see the
// statusMsg assignments) so the visual category matches the meaning.
func notificationStyleFor(text string) (glyph, color string) {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "copied"):
		return glyphCheck, "82" // bright green — quick positive confirmation
	case strings.Contains(lower, "guidance") && !strings.Contains(lower, "full"):
		return glyphArrowInto, "39" // blue — steering injected into the active run
	case strings.Contains(lower, "full") || strings.Contains(lower, "failed") || strings.Contains(lower, "try again"):
		return glyphWarning, "214" // amber — recoverable problem
	case strings.Contains(lower, "cancel"):
		return glyphCross, "203" // red — run aborted
	default:
		return glyphBullet, "245" // neutral grey — generic notice
	}
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

// openHistory opens a scrollable, full-fidelity view of the conversation. In
// hybrid mode, committed cells live in immutable scrollback with bounded diffs;
// this overlay is the "expand the full diff / review past turns" escape hatch
// the immutable model otherwise gives up.
func (m *uiModel) openHistory() {
	width := m.width
	if width <= 0 {
		width = 80
	}
	m.pager = components.NewPager(m.common, width, m.height, m.renderHistoryContent)
}

func (m *uiModel) renderHistoryContent(width int) string {
	if width < 20 {
		width = 20
	}
	if len(m.messages) == 0 {
		return "No conversation history yet."
	}
	var sb strings.Builder
	for i := range m.messages {
		msg := m.messages[i]
		var rendered string
		// Patches render with an effectively unbounded diff here (the inline
		// view bounds them; this overlay is where you see the whole change).
		if msg.ToolName == "patch" && !msg.IsError {
			if patch := patchArgOf(msg.ToolArgs); strings.TrimSpace(patch) != "" {
				rendered = renderPatchCell(patch, msg.Duration, width, 1<<30)
			}
		}
		if rendered == "" {
			rendered = renderCell(msg, width)
		}
		if rendered = strings.TrimRight(rendered, "\n"); rendered == "" {
			continue
		}
		sb.WriteString(rendered + "\n\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func patchArgOf(toolArgs string) string {
	var a map[string]interface{}
	if json.Unmarshal([]byte(toolArgs), &a) == nil {
		if p, ok := a["patch"].(string); ok {
			return p
		}
	}
	return ""
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
	header := m.displayModelName()
	if meta := strings.TrimSpace(m.modelMeta); meta != "" {
		header += " " + meta
	}
	cwd := currentWorkingDir()

	parts := []string{
		st.Status.Value.Render(header),
		st.Status.Value.Render(cwd),
		st.Status.Label.Render(formatUsageSession(m.runTokens, m.totalTokens, m.tokenLimit)),
	}

	state := m.runStatus
	stateStyle := st.Status.Good
	switch {
	case m.approvalFlowActive():
		// A pending approval is a distinct state — the run is paused on the
		// user, not "working", and definitely not "ready".
		state = "⏸ waiting approval"
		stateStyle = st.Status.Warning
	case m.runStatus == "working" && !m.thinkingStart.IsZero():
		state = fmt.Sprintf("working %.1fs", time.Since(m.thinkingStart).Seconds())
	}
	parts = append(parts, stateStyle.Render(state))

	// Show the approval mode unless it's the default, so an elevated mode
	// (auto-edit / full-auto) is always visible.
	if m.approvalMode != "" && m.approvalMode != "on-request" {
		parts = append(parts, st.Status.Label.Render("mode:"+m.approvalMode))
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

// Update wraps updateInner so any cells committed during the handler are
// flushed to scrollback as tea.Println Cmds *after* the handler returns. This
// keeps Program.Println off the Update goroutine (which would deadlock).
func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.updateInner(msg)
	return model, m.flushPendingPrintln(cmd)
}

func (m *uiModel) updateInner(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		// Hybrid: commit the startup card to scrollback once, now that the width
		// is known, so it persists at the top of history (like Codex) instead of
		// vanishing when the first message scrolls the active region.
		if m.hybrid && !m.startupCommitted && msg.Width > 0 {
			m.startupCommitted = true
			if card := strings.TrimRight(strings.Join(m.renderStartupCard(msg.Width), "\n"), "\n"); card != "" {
				m.pendingPrintln = append(m.pendingPrintln, card)
			}
		}
		// Attach digest waits for the first sized frame so it lands after the
		// startup card and renders at a real width in both renderers.
		return m, m.maybeShowStartupDigest(msg.Width)

	case spinner.TickMsg:
		return m, spinnerCmd

	case MsgWorkingTick:
		if m.thinking || m.toolExecuting != "" {
			m.thinkingDots++
			return m, workingTick()
		}
		return m, nil

	case MsgCursorBlinkTick:
		m.cursorVisible = !m.cursorVisible
		if m.editor != nil {
			m.editor.SetCursorVisible(m.cursorVisible)
		}
		return m, cursorBlinkTick()

	case MsgStreamFlush:
		m.streamFlushPending = false
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		if m.flushLiveStreamPending() {
			m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		}
		return m, m.scheduleStreamFlush()

	case MsgMouseAutoScrollTick:
		m.mouseScrollTicking = false
		if !m.mouseDragActive || m.mouseAutoScrollDir == 0 {
			return m, nil
		}
		m.scrollTranscriptLines(m.mouseAutoScrollDir)
		m.updateMouseSelectionAfterAutoScroll()
		return m, m.ensureMouseAutoScroll()

	case MsgAgentActivity:
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		m.activityText = strings.TrimSpace(msg.Content)
		m.thinking = true
		if m.thinkingStart.IsZero() {
			m.thinkingStart = time.Now()
		}
		m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		return m, spinnerCmd

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
		return m.handleMouse(msg)

	case MsgStream:
		msg.Content = textutil.CleanUTF8(msg.Content)
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		m.thinking = false
		m.activityText = ""
		if committed := m.streamController.Push(msg.Content); committed != "" {
			m.commitLiveStream(committed)
			m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		}
		return m, m.scheduleStreamFlush()

	case MsgAgentDone:
		m.exitPromptActive = false
		// The run is over; any unanswered approval row is expired by the daemon
		// once its waiter is gone, so drop stale approval UI instead of letting a
		// panel answer into the void.
		m.clearApprovalFlow()
		msg.Response = textutil.CleanUTF8(msg.Response)
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		m.thinking = false
		m.activityText = ""
		m.toolExecuting = ""
		m.steerCh = nil // run finished; stop accepting mid-turn guidance for it
		// The mid-turn guidance notice is stale once the run ends — clear it.
		if strings.HasPrefix(m.statusMsg, "Sent to the running task") || strings.Contains(m.statusMsg, "Guidance queue") {
			m.statusMsg = ""
		}
		turnTokens := msg.Usage.InputTokens + msg.Usage.OutputTokens
		if turnTokens > 0 {
			m.runTokens = turnTokens
			m.totalTokens += turnTokens
		}
		if msg.Err != nil {
			m.runStatus = "error"
			m.finalizeLiveStream(msg.Response)
			m.addErrorMessage(fmt.Sprintf("Error: %v", msg.Err))
		} else if m.finalizeLiveStream(msg.Response) {
			m.runStatus = "done"
		} else {
			m.runStatus = "error"
			m.addErrorMessage("Error: model returned an empty response without any error details. Check the provider credentials and endpoint, then retry.")
		}
		m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		return m, spinnerCmd

	case MsgAttachedRunDone:
		// The watched (re-attached) daemon run ended. Unlike MsgAgentDone there
		// is no synchronous answer to finalize — the conversation reply follows
		// the run's ORIGIN endpoint (docs/identity-continuity.md); this watcher
		// only reports that the observation ended and the recorded outcome.
		wasWatching := m.watchingRun
		m.watchingRun = false
		m.watchedTaskTitle = ""
		m.watchCancel = nil
		if strings.HasPrefix(m.statusMsg, "Watching ") {
			m.statusMsg = ""
		}
		if msg.Cancelled {
			// The user detached (ctrl+c during watch) — the run keeps running on
			// the daemon; just stop reporting. No "finished" line.
			if wasWatching {
				m.statusMsg = "Detached — the task keeps running in the background."
			}
			return m, spinnerCmd
		}
		if summary := strings.TrimSpace(msg.Summary); summary != "" {
			m.addMessage("system", "The running task finished: "+summary)
		} else {
			m.addMessage("system", "The running task finished. Use /status for details.")
		}
		return m, spinnerCmd

	case MsgApprovalRequest:
		// The daemon is blocked waiting for approval. Arm the interactive panel
		// (active region); if one is already up, queue FIFO and re-arm after the
		// current decision. No redundant text notice — the panel is the prompt.
		if m.approvalFlowActive() {
			m.approvalQueue = append(m.approvalQueue, msg)
			return m, nil
		}
		m.armApprovalPrompt(msg)
		return m, nil

	case MsgToolStart:
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		m.finalizeLiveStream("")
		m.thinking = false
		m.activityText = ""
		m.toolExecuting = msg.ToolName
		m.addMessage("tool", "")
		last := &m.messages[len(m.messages)-1]
		last.ToolName = msg.ToolName
		last.ToolCallID = msg.ToolCallID
		last.ToolArgs = msg.Args
		last.IsRunning = true
		m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		return m, spinnerCmd

	case MsgToolDone:
		msg.Result = textutil.CleanUTF8(msg.Result)
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		idx := m.findToolMessageIndex(msg.ToolCallID, msg.ToolName)
		if idx >= 0 {
			last := &m.messages[idx]
			last.ToolName = msg.ToolName
			if msg.ToolCallID != "" {
				last.ToolCallID = msg.ToolCallID
			}
			last.Duration = msg.Duration
			last.IsRunning = false
			last.RunningDetail = ""
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
			// The tool cell is now final — commit it to scrollback (hybrid mode).
			m.commit(last)
		}
		if !m.anyToolRunning() {
			m.toolExecuting = ""
		}
		m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		return m, spinnerCmd

	case MsgToolOutput:
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		m.thinking = false
		m.activityText = ""
		m.appendToolOutput(msg.ToolName, msg.Content)
		m.restoreOrFollowTranscript(wasAtBottom, yOffset)
		return m, spinnerCmd

	case MsgToolHeartbeat:
		if msg.ToolName != "" {
			m.toolExecuting = msg.ToolName
		}
		idx := m.findToolMessageIndex(msg.ToolCallID, msg.ToolName)
		if idx >= 0 {
			last := &m.messages[idx]
			if msg.ToolName == "" || last.ToolName == "" || last.ToolName == msg.ToolName {
				if msg.ToolName != "" {
					last.ToolName = msg.ToolName
				}
				if msg.ToolCallID != "" {
					last.ToolCallID = msg.ToolCallID
				}
				if !isGenericToolHeartbeat(msg.ToolName, msg.Content) {
					last.RunningDetail = msg.Content
				}
			}
		}
		return m, spinnerCmd

	case MsgLearningEvent:
		wasAtBottom := m.transcriptAtBottom()
		yOffset := m.viewport.YOffset
		m.statusMsg = msg.Content
		m.addMessage("system", msg.Content)
		m.restoreOrFollowTranscript(wasAtBottom, yOffset)
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
	// An active approval panel captures every key except ctrl+c: the decision
	// must be explicit (Esc does nothing), and stray keys must not leak into
	// the composer.
	if m.approvalPrompt != nil {
		if cmd, handled := m.handleApprovalPromptKey(msg); handled {
			return m, cmd
		}
	}

	// Ctrl+V, or an empty bracketed paste (how a screenshot paste arrives — the
	// image has no text payload), may be an image paste. Check the clipboard for
	// an image and route it to the attachment pipeline. If there is no image it
	// stays silent: an empty paste is swallowed; Ctrl+V falls through to normal
	// handling so text paste is unaffected.
	if msg.Type == tea.KeyCtrlV || (msg.Paste && strings.TrimSpace(string(msg.Runes)) == "") {
		if cmd, handled := m.tryClipboardImagePaste(); handled {
			return m, cmd
		}
		if msg.Paste {
			return m, nil
		}
	}

	switch msg.Type {

	// Shift+Enter or Ctrl+J inserts a newline (multi-line input).
	case tea.KeyCtrlJ:
		m.editor.Update(msg)
		return m, nil
	case tea.KeyPgUp:
		m.scrollTranscriptPage(-1)
		return m, nil
	case tea.KeyPgDown:
		m.scrollTranscriptPage(1)
		return m, nil
	case tea.KeyCtrlUp:
		m.scrollTranscriptLines(-3)
		return m, nil
	case tea.KeyCtrlDown:
		m.scrollTranscriptLines(3)
		return m, nil
	case tea.KeyCtrlHome:
		m.syncTranscriptContent()
		m.viewport.GotoTop()
		return m, nil
	case tea.KeyCtrlEnd:
		m.syncTranscriptContent()
		m.viewport.GotoBottom()
		return m, nil
	case tea.KeyUp:
		// While the slash-command popup is open, Up/Down navigate it (codex-style)
		// instead of input history.
		if m.editor.SuggestionsVisible() {
			m.editor.MoveSuggestion(-1)
			return m, nil
		}
		if m.navigateInputHistory(-1) {
			return m, nil
		}
		m.editor.Update(msg)
		return m, nil
	case tea.KeyDown:
		if m.editor.SuggestionsVisible() {
			m.editor.MoveSuggestion(1)
			return m, nil
		}
		if m.navigateInputHistory(1) {
			return m, nil
		}
		m.editor.Update(msg)
		return m, nil
	case tea.KeyTab:
		// Tab completes the highlighted slash command.
		if m.editor.AcceptSuggestion() {
			return m, nil
		}
		m.editor.Update(msg)
		return m, nil

	default:
		if m.exitPromptActive {
			if cmd, handled := m.handleExitPromptKey(msg.String()); handled {
				return m, cmd
			}
		}
		switch msg.String() {
		case "esc":
			return m, nil
		case "ctrl+c":
			// Priority 1: if input has content, clear it (don't quit)
			if input := m.editor.Value(); input != "" {
				m.editor.Reset()
				return m, nil
			}
			// Priority 1.5: passively watching a daemon run (not our turn).
			// ctrl+c leaves the spectator view — the run keeps running on the
			// daemon; it is not the "cancel my task" prompt.
			if m.watchingRun {
				if m.watchCancel != nil {
					m.watchCancel()
				}
				return m, nil
			}
			// Priority 2: a run is active. Runs are daemon-owned (G0-a), so
			// quitting does NOT cancel — offer the choice explicitly. This
			// prompt doubles as the moment the user learns the detached-run
			// design. A second ctrl+c means "background + quit".
			if m.thinking || m.toolExecuting != "" {
				if m.exitPromptActive {
					return m, tea.Quit
				}
				m.exitPromptActive = true
				m.addMessage("assistant", "A task is still running. Choose:\n  b — quit and leave it running in the background (the result will be pushed to your bound IM)\n  c — cancel the task and stay\n  esc — keep watching")
				return m, nil
			}
			// Priority 3: quit
			return m, tea.Quit
		case "ctrl+l":
			m.messages = []ChatMessage{}
			m.clearLiveStream()
			m.viewport.SetContent("")
			return m, m.clearHybridScreen()
		case "enter":
			// Deny follow-up (approval panel "No"): Enter resolves it — bare Enter
			// is a plain deny, typed text is deny + guidance. Checked before the
			// empty-input early return so bare Enter works.
			if m.approvalDenyFollowup {
				return m, m.finishApprovalDeny(m.editor.ExpandValue())
			}
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
				m.runTokens = 0
				m.activityText = "Thinking about the response"
				return m, tea.Batch(m.spinner.Tick, workingTick())
			}

			if strings.HasPrefix(input, "/") {
				return m, m.handleCommand(input)
			}
			// Mid-turn steering: if a run is active, inject this as guidance into
			// the running turn instead of starting a competing run (which the
			// busy-guard would reject) or overwriting the in-flight cancelFn.
			if m.thinking || m.toolExecuting != "" {
				return m, m.injectMidRunGuidance(input)
			}
			m.addMessage("user", input)
			m.editor.Reset()
			m.steerCh = make(chan string, 16)
			m.thinking = true
			m.runStatus = "working"
			m.thinkingStart = time.Now()
			m.thinkingDots = 0
			m.runTokens = 0
			m.activityText = "Thinking about the request"
			ctx, cancel := context.WithCancel(context.Background())
			m.cancelFn = cancel
			ctx = kernel.WithSteering(ctx, m.steerCh)
			return m, tea.Batch(m.runAgent(ctx, input), m.spinner.Tick, workingTick())
		}
	}
	m.editor.Update(msg)
	return m, nil
}

// injectMidRunGuidance routes input typed while a run is in flight into that
// run as user guidance. In-process runs receive it through the local steering
// channel (kernel.WithSteering); in client mode the run executes inside the
// daemon process, so the guidance must be forwarded over the gateway API — a
// local channel push would be silently dropped. Failures are surfaced as an
// explicit transcript notice: guidance must never appear accepted when it
// was not.
func (m *uiModel) injectMidRunGuidance(input string) tea.Cmd {
	m.addMessage("user", input)
	m.editor.Reset()
	switch {
	case m.clientMode && m.steerFn != nil:
		if err := m.steerFn(input); err != nil {
			m.addErrorMessage(fmt.Sprintf("The daemon did not accept the guidance: %v", err))
			m.statusMsg = "Guidance was not accepted by the daemon."
		} else {
			m.statusMsg = "Sent to the running task as guidance."
		}
	case m.steerCh != nil:
		select {
		case m.steerCh <- input:
			m.statusMsg = "Sent to the running task as guidance."
		default:
			m.statusMsg = "Guidance queue is full; try again in a moment."
		}
	}
	// Transient notice — auto-clear so it doesn't linger after the turn.
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return MsgClearStatus{} })
}

func (m *uiModel) syncTranscriptContent() {
	if m == nil {
		return
	}
	m.viewport.SetContent(m.renderAllMessages())
}

func (m *uiModel) scrollTranscriptPage(direction int) {
	m.syncTranscriptContent()
	if direction < 0 {
		m.viewport.PageUp()
		return
	}
	if direction > 0 {
		m.viewport.PageDown()
	}
}

func (m *uiModel) scrollTranscriptLines(lines int) {
	m.syncTranscriptContent()
	if lines < 0 {
		m.viewport.LineUp(-lines)
		return
	}
	if lines > 0 {
		m.viewport.LineDown(lines)
	}
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

type MsgAgentDone struct {
	Response string
	Usage    llm.UsageStats
	Err      error
}

type MsgStream struct {
	Content string
}

type MsgToolStart struct {
	ToolName   string
	ToolCallID string
	Args       string
}

type MsgToolOutput struct {
	ToolName string
	Content  string
}

type MsgToolHeartbeat struct {
	ToolName   string
	ToolCallID string
	Content    string
}

type MsgToolDone struct {
	ToolName   string
	ToolCallID string
	Result     string
	Err        error
	Duration   float64
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
	{Left: "PageUp/PageDown", Right: "Scroll the transcript by one page"},
	{Left: "Ctrl+Up/Ctrl+Down", Right: "Scroll the transcript by a few lines"},
	{Left: "Ctrl+Home/Ctrl+End", Right: "Jump to the top or bottom of the transcript"},
	{Left: "Mouse wheel", Right: "Scroll the transcript when the pointer is over chat"},
	{Left: "Mouse drag edge", Right: "Auto-scroll while dragging near the transcript edge"},
	{Left: "Ctrl+C", Right: "Clear input, cancel a running task, close help, or exit"},
	{Left: "Ctrl+L", Right: "Clear the current transcript view"},
	{Left: "Shift+mouse drag", Right: "Use terminal-native text selection when mouse mode is active"},
}

var _ tea.Model = (*uiModel)(nil)

// handleExitPromptKey resolves the quit-with-active-run prompt. handled=false
// lets unrelated keys fall through (they are ignored with a re-hint rather
// than typed into the editor, so a stray keystroke cannot half-answer).
func (m *uiModel) handleExitPromptKey(key string) (tea.Cmd, bool) {
	switch key {
	case "b", "B":
		m.exitPromptActive = false
		return tea.Quit, true
	case "c", "C":
		m.exitPromptActive = false
		return m.cancelActiveRunLocally(), true
	case "esc":
		m.exitPromptActive = false
		m.statusMsg = "Still watching."
		return nil, true
	case "ctrl+c":
		// Handled by the ctrl+c case itself (second ctrl+c = background+quit).
		return nil, false
	default:
		m.statusMsg = "Press b (background + quit), c (cancel + stay), or esc."
		return nil, true
	}
}

// cancelActiveRunLocally detaches the local watcher UI state and routes the
// actual cancellation through the registry-backed /stop control command
// (runs are daemon-owned since G0-a; the legacy in-process agent path still
// cancels through the local ctx).
func (m *uiModel) cancelActiveRunLocally() tea.Cmd {
	if m.cancelFn == nil {
		return nil
	}
	m.cancelFn()
	m.finalizeLiveStream("")
	m.thinking = false
	m.activityText = ""
	m.toolExecuting = ""
	m.steerCh = nil
	m.runStatus = "cancelled"
	m.statusMsg = "Task cancelled by user."
	return tea.Batch(m.requestDaemonStop(), tea.Tick(time.Second*3, func(t time.Time) tea.Msg {
		return MsgClearStatus{}
	}))
}
