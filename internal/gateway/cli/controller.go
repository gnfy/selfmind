package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"selfmind/internal/app"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/components"
	"selfmind/internal/ui/components/sidebar"
	"selfmind/internal/ui/components/status"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
)

// Controller wraps the Bubble Tea program.
type Controller struct {
	model     *uiModel
	cleanupFn func()
}

type MessageProcessor func(context.Context, api.MessageRequest) (api.MessageResponse, int)
type EventWatcher func(context.Context, httpapi.StreamObserver, func(api.RunEvent))

// ChatMessage represents a single message in the conversation history.
type ChatMessage struct {
	Role          string // "user", "assistant", "system", "tool"
	Content       string
	Timestamp     time.Time
	ToolName      string // populated when Role == "tool"
	ToolCallID    string
	RunID         string  // run that owns this tool call; prevents late cross-run completion routing
	ToolArgs      string  // Fix: add ToolArgs to store call arguments
	Duration      float64 // Fix: add Duration for performance display
	IsError       bool    // Fix: add IsError flag
	IsRunning     bool
	RunningDetail string
	NoticeKind    noticeKind // structured semantics for notice-role cells; never inferred from prose
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
	eventWatcher       EventWatcher
	eventWatchCancel   context.CancelFunc
	tenantID           string
	channel            string // 'cli' | 'wechat' | 'dingtalk' | 'web'
	approvalMode       string // codex-style session override: on-request | read-only | auto-edit | full-auto | smart. In client mode "" means "defer to the person's persisted mode".
	// persistedApprovalMode is the person's daemon-side effective mode, learned
	// from GET /v1/digest at startup. It backs the status-bar display when the
	// session has no explicit override (approvalMode == ""), so the bar always
	// shows the real effective mode instead of nothing.
	persistedApprovalMode string
	spinner               spinner.Model
	inputHistory          []string
	inputHistoryStore     *inputHistoryStore // nil when persistence is disabled
	historyIndex          int
	historyDraft          string
	clarifyBridge         *tools.ClarifyBridge
	updateNotices         <-chan UpdateNotice // one-shot background update-check result (update_notice.go); nil after consumption
	updateNoticeAnnounced string              // version already announced (startup notice or in-session), the dedup key
	cancelFn              context.CancelFunc
	steerCh               chan string // mid-turn guidance channel for the active run (nil when idle)
	clarifyMode           bool
	clarifyChoices        []string
	clarifyReq            tools.ClarifyRequest
	clarifyGateway        bool
	secretMode            bool
	secretKey             string
	statusMsg             string // Transient status message text (kept separate from its structured kind)
	statusNoticeText      string // text paired with statusNoticeKind; mismatches render as neutral info
	statusNoticeKind      noticeKind
	statusNoticeID        uint64
	nextStatusNoticeID    uint64
	thinkingDots          int       // Counter for "..." animation
	thinkingStart         time.Time // When current thinking started
	activityText          string    // Current model/tool phase shown in transcript
	activePlanJSON        string    // Latest complete plan snapshot, rendered above the composer
	runStatus             string    // ready | queued | working | done | error | cancelled
	queuedCount           int       // requests submitted by this TUI and accepted into the daemon queue
	queuedInputs          []string  // local queue acknowledgements awaiting run.started
	localRequestActive    bool      // this TUI currently owns a synchronous POST
	localRequestInput     string
	daemonRunActive       bool // person-level daemon stream reports an executing run
	daemonRunID           string
	daemonRunStarted      time.Time
	daemonRunAwaitingDone bool // final answer still arrives through MsgAgentDone
	// backgroundRunID is the daemon run whose progress this terminal must NOT
	// render: the daemon started it on the person's behalf (a watcher
	// finalization, a cron fire). backgroundOrigin names that initiator and
	// backgroundWatchID is set only for a watcher; backgroundResultPending
	// tracks the one result line the run still owes. See markBackgroundRun in
	// daemon_events.go.
	backgroundRunID         string
	backgroundWatchID       string
	backgroundOrigin        string
	backgroundResultPending bool
	migrationHint           string // Hint for migrating Hermes skills
	streamController        markdownStreamController
	liveStreamContent       string
	streamFlushPending      bool
	cursorVisible           bool
	clientMode              bool // daemon-client mode: no in-process agent/gateway; chat routes to the daemon
	// workspaceOverride* pin this session's execution workspace after a
	// successful `/workspace <n|id>` switch (mirrors the approvalMode session
	// override). Without it the next CLI turn silently re-derived a workspace
	// from the launch cwd — /workspace was a no-op for subsequent messages
	// (observed live). While set, every agent-bound and control request from
	// this session carries WorkspaceID (explicit workspace wins server-side)
	// and the status bar shows the override instead of the cwd. Empty = the
	// default cwd-derived behavior.
	workspaceOverrideID   string
	workspaceOverrideName string
	workspaceOverridePath string
	toolDispatchFn        func(tool string, args map[string]interface{}) (string, error) // client mode: run management tools on the daemon
	approvalResponder     func(approvalID, decision, scope, grantKey string) error       // client mode: answer a daemon tool-approval request (scope is daemon-issued; grantKey is a rule the ask offered)
	steerFn               func(text string) error                                        // client mode: forward mid-turn guidance to the daemon's active run
	// Interactive approval panel state (see approval_flow.go). approvalPrompt is
	// the active panel; approvalQueue holds requests that arrived while one was
	// already up (FIFO re-arm).
	approvalPrompt *components.ApprovalPrompt
	approvalQueue  []MsgApprovalRequest
	// delayedApprovals hold requests that arrived while the person was typing;
	// they are armed once input goes idle (approvalTypingIdleDelay) so a panel
	// cannot swallow an in-flight keystroke as a decision.
	delayedApprovals    []MsgApprovalRequest
	lastInputActivityAt time.Time
	pendingApprovalID   string
	pendingApprovalTool string
	// Successful local answers are remembered until their person-stream echo
	// arrives. The echo still reconciles queue state, but must not create a
	// duplicate "resolved elsewhere" transcript record.
	localApprovalResolutions map[string]string
	localApprovalOrder       []string
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
	watchedRunID     string
	watchedTaskTitle string
	watchCancel      context.CancelFunc
	seenEventKeys    map[string]struct{}
	seenEventOrder   []string
	// exitPromptActive intercepts keys while the quit-with-active-run prompt
	// is shown (b = background+quit, c = cancel+stay, esc = keep watching).
	exitPromptActive bool
	// quitting is set by quitNow on every quit path so the final repaint
	// clears the active region instead of stranding an empty composer between
	// the transcript and the shell prompt.
	quitting bool
	// resumePickerArmed is set by a bare /resume: the daemon-rendered task list
	// becomes a menu, so the next bare number is read as "resume that entry"
	// and expanded to /resume <n>. Resolution stays on the daemon, which owns
	// the ordering that produced the list.
	resumePickerArmed bool
	hybrid            bool     // terminal-first hybrid mode (SELFMIND_TUI_HYBRID)
	pendingPrintln    []string // hybrid: cells to emit to scrollback at end of Update
	startupCommitted  bool     // hybrid: startup card already printed to scrollback
}

type MsgClearStatus struct{ NoticeID uint64 }
type MsgWorkingTick time.Time
type MsgCursorBlinkTick time.Time
type MsgStreamFlush time.Time
type MsgAgentActivity struct {
	Content string
	Event   uiEventRef
}

// MsgApprovalRequest is emitted (client mode) when the daemon blocks a run
// waiting for tool approval. The TUI renders the interactive approval panel in
// the active region and answers via the approval responder.
type MsgApprovalRequest struct {
	ID     string
	Tool   string
	Target string // compact object of the action (path/command); may be empty
	Reason string
	// WaiterState is "live" for an in-flight blocked run and "parked" when the
	// old run released its resources; answering a parked request resumes task.
	WaiterState string
	// Decision context published with the approval.requested event: where the
	// operation would run, how large the write is, what a run-local reuse answer
	// authorizes, and whether smart-mode triage could rule at all. All optional —
	// an older daemon sends none of it and the panel simply omits those lines.
	Environment   string
	Cwd           string
	ChangeSummary string
	GrantClass    string
	Containment   string
	TriageState   string
	CodePreview   string
	CodeSHA256    string
	CodeLines     int
	CodeBytes     int
	// Rationale and Risk are the judge's assessment when triage ran and handed
	// the call over; Options is the daemon's authoritative answer set for this
	// ask (nil for an older daemon, which falls back to the built-in options).
	Rationale string
	Risk      string
	Options   []components.ApprovalOption
}

// MsgApprovalResolved closes a matching approval panel or queued request when
// another client answered it or the daemon expired it. Approved, rejected, and
// expired are durable events; clients reconcile by id instead of leaving a
// stale modal behind.
type MsgApprovalResolved struct {
	ID         string
	Status     string
	Scope      string
	DecisionID string
	Event      uiEventRef
}

// MsgApprovalParked keeps a matching request answerable while explaining that
// its original run released resources and a decision will start continuation.
type MsgApprovalParked struct {
	ID    string
	Event uiEventRef
}

// MsgClarifyRequest is emitted in daemon-client mode when a remote run blocks
// on a clarify question. The answer is posted back through the gateway as a
// normal message, so the original run keeps executing and the live event poller
// keeps showing progress.
type MsgClarifyRequest struct {
	ID       string
	Question string
	Choices  []string
}

type MsgClarifyAnswerResult struct {
	Err error
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
func (c *Controller) SetMessageProcessor(processor MessageProcessor) {
	if c == nil || c.model == nil {
		return
	}
	c.model.messageProcessor = processor
}

// SetEventWatcher installs the person-scoped daemon stream used for the whole
// TUI lifetime. It is deliberately separate from MessageProcessor: a queued
// POST may return before its run starts, while this stream remains attached.
func (c *Controller) SetEventWatcher(watcher EventWatcher) {
	if c == nil || c.model == nil {
		return
	}
	c.model.eventWatcher = watcher
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
	if enabled {
		// In client mode the daemon owns the effective approval mode (the
		// person's persisted /mode). Clear the local default so requests defer
		// to it; the status bar shows the persisted value fetched from the digest
		// until the user sets a session override with /mode.
		c.model.approvalMode = ""
	}
}

// SetPersistedApprovalMode records the person's daemon-side effective approval
// mode (from GET /v1/digest) so the status bar can show it from startup, before
// any /mode override. Empty input is ignored.
func (c *Controller) SetPersistedApprovalMode(mode string) {
	if c == nil || c.model == nil {
		return
	}
	if m := strings.TrimSpace(mode); m != "" {
		c.model.persistedApprovalMode = m
	}
}

// effectiveApprovalMode is the mode shown in the status bar and used for
// display: a session override wins, else the persisted mode from the digest,
// else the smart product default.
func (m *uiModel) effectiveApprovalMode() string {
	if mode := strings.TrimSpace(m.approvalMode); mode != "" {
		return mode
	}
	if mode := strings.TrimSpace(m.persistedApprovalMode); mode != "" {
		return mode
	}
	return string(tools.DefaultApprovalMode)
}

// SetApprovalResponder installs the function used to answer a daemon
// tool-approval request from the client TUI's approval panel. scope carries the
// daemon-issued grant scope through the existing
// /v1/approvals/respond path. Only set in client mode; in-process approvals use
// the clarify bridge instead.
func (c *Controller) SetApprovalResponder(fn func(approvalID, decision, scope, grantKey string) error) {
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

// HasConversationHistory reports whether THIS session contains a user turn
// worth resuming. A freshly opened-and-closed TUI gets a session id for
// isolation, but should not advertise a useless resume command. The signal is
// a user-role message in the transcript — every submit path (chat, command
// echo, clarify answer) adds one. inputHistory is deliberately NOT consulted:
// since persistent input history (2026-07-20) it is pre-seeded from
// ~/.selfmind/input_history.jsonl at startup, so its length reflects all-time
// typing, not this session (the regression that made the hint print on every
// zero-input exit). Assistant/system startup content (digest, notices) must
// not trigger the hint either.
func (c *Controller) HasConversationHistory() bool {
	if c == nil || c.model == nil {
		return false
	}
	for _, msg := range c.model.messages {
		if msg.Role == "user" && strings.TrimSpace(msg.Content) != "" {
			return true
		}
	}
	return false
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
func (m *uiModel) scheduleStreamFlush() tea.Cmd {
	if m.streamFlushPending || !m.streamController.Pending() {
		return nil
	}
	m.streamFlushPending = true
	return streamFlushTick()
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
func patchArgOf(toolArgs string) string {
	var a map[string]interface{}
	if json.Unmarshal([]byte(toolArgs), &a) == nil {
		if p, ok := a["patch"].(string); ok {
			return p
		}
	}
	return ""
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
	kind := noticeGuidance
	status := ""
	switch {
	case m.clientMode && m.steerFn != nil:
		if err := m.steerFn(input); err != nil {
			m.addErrorMessage(fmt.Sprintf("The daemon did not accept the guidance: %v", err))
			kind = noticeWarning
			status = "Guidance was not accepted by the daemon."
		} else {
			status = "Sent to the running task as guidance."
		}
	case m.steerCh != nil:
		select {
		case m.steerCh <- input:
			status = "Sent to the running task as guidance."
		default:
			kind = noticeWarning
			status = "Guidance queue is full; try again in a moment."
		}
	}
	// Transient notice — auto-clear so it doesn't linger after the turn.
	id := m.setStatusNotice(kind, status)
	return clearStatusNoticeAfter(id, 3*time.Second)
}

func (m *uiModel) armClarifyPrompt(req tools.ClarifyRequest, viaGateway bool) {
	m.thinking = false
	m.toolExecuting = ""
	m.clarifyMode = true
	m.clarifyGateway = viaGateway
	m.clarifyChoices = req.Choices
	m.addMessage("assistant", fmt.Sprintf("Clarification needed: %s", req.Question))
	if len(req.Choices) > 0 {
		var lines []string
		for i, c := range req.Choices {
			lines = append(lines, fmt.Sprintf("  %d. %s", i+1, c))
		}
		lines = append(lines, "  0. Other (type your answer)")
		m.addMessage("assistant", "Options:\n"+strings.Join(lines, "\n"))
	}
	m.clarifyReq = req
	m.setStatusNotice(noticeWarning, "Answer the question to continue the task.")
}

func (m *uiModel) answerClarifyViaGateway(response string) tea.Cmd {
	if m.messageProcessor == nil {
		return func() tea.Msg { return MsgClarifyAnswerResult{Err: fmt.Errorf("gateway is not initialized")} }
	}
	processor := m.messageProcessor
	req := m.controlMessageRequest(response)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, status := processor(ctx, req)
		if resp.Error != "" {
			return MsgClarifyAnswerResult{Err: fmt.Errorf("%s", resp.Error)}
		}
		if status >= 400 {
			return MsgClarifyAnswerResult{Err: fmt.Errorf("gateway returned HTTP %d", status)}
		}
		return MsgClarifyAnswerResult{}
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
	// A graceful shutdown repaints the model one final time, so the active
	// region has to render itself away or the composer stays on screen above
	// the resume hint. One blank line rather than "": the renderer skips a
	// frame whose buffer is empty, and its stop() erases exactly the last
	// rendered line — so a single line both clears the region below and
	// removes itself.
	if m.quitting {
		return " "
	}
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	return m.viewModel()
}

type MsgAgentDone struct {
	Response string
	Usage    llm.UsageStats
	Err      error
	Input    string
	Turn     *api.TurnStatus
}

type MsgDaemonRunStarted struct {
	RunID      string
	TaskID     string
	WatchID    string
	TaskStatus string
	// Origin is set when the daemon started this run on the person's behalf
	// (a watcher finalization, a cron fire) rather than from a turn they typed
	// at an endpoint. Empty for the person's own work, wherever they typed it.
	Origin  string
	Input   string
	Started time.Time
	Event   uiEventRef
}

type MsgDaemonRunFinished struct {
	RunID   string
	Status  string
	Summary string
	Event   uiEventRef
}

type MsgStream struct {
	Content string
	Event   uiEventRef
}

type MsgToolStart struct {
	ToolName   string
	ToolCallID string
	Args       string
	Event      uiEventRef
}

type MsgToolOutput struct {
	ToolName   string
	ToolCallID string
	Content    string
	Event      uiEventRef
}

type MsgToolHeartbeat struct {
	ToolName   string
	ToolCallID string
	Content    string
	Event      uiEventRef
}

type MsgToolDone struct {
	ToolName   string
	ToolCallID string
	Result     string
	Err        error
	Duration   float64
	Event      uiEventRef
}

// MsgPlanUpdated replaces the live plan shown above the composer. Plan events
// are snapshots rather than transcript cells: only the latest snapshot is
// useful while the run is active, and finalized terminal history stays clean.
type MsgPlanUpdated struct {
	Content string
	Event   uiEventRef
}

type MsgLearningEvent struct {
	Content string
	Event   uiEventRef
}

type MsgWatcherCompleted struct {
	WatchID    string
	Status     string
	TaskStatus string
	Event      uiEventRef
}

// MsgTokens is a live cumulative token-usage snapshot for the active run,
// derived from the daemon's token.updated events. Run is input+output tokens
// so far; it updates the status bar mid-run while the final response usage
// stays authoritative for session totals.
type MsgTokens struct {
	Run   int
	Event uiEventRef
}

// MsgWorkspaceSwitched reports a successful gateway /workspace switch, carrying
// the resolved workspace parsed from the control reply plus the raw reply text
// to render. A failed switch never produces this message (the reply renders as
// a plain MsgAgentDone and no override is set).
type MsgWorkspaceSwitched struct {
	ID    string
	Name  string
	Path  string
	Reply string
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
// quitNow records that a quit path was taken and returns the quit command.
// Every exit route goes through it so View can blank the active region on the
// final repaint.
func (m *uiModel) quitNow() tea.Cmd {
	m.quitting = true
	return tea.Quit
}

func (m *uiModel) handleExitPromptKey(key string) (tea.Cmd, bool) {
	switch key {
	case "b", "B":
		m.exitPromptActive = false
		return m.quitNow(), true
	case "c", "C":
		m.exitPromptActive = false
		return m.cancelActiveRunLocally(), true
	case "esc":
		m.exitPromptActive = false
		m.setStatusNotice(noticeInfo, "Still watching.")
		return nil, true
	case "ctrl+c":
		// Handled by the ctrl+c case itself (second ctrl+c = background+quit).
		return nil, false
	default:
		m.setStatusNotice(noticeInfo, "Press b (background + quit), c (cancel + stay), or esc.")
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
	m.activePlanJSON = ""
	m.steerCh = nil
	m.runStatus = "cancelled"
	noticeID := m.setStatusNotice(noticeError, "Task cancelled by user.")
	return tea.Batch(m.requestDaemonStop(), clearStatusNoticeAfter(noticeID, 3*time.Second))
}
