package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *uiModel) updateInner(msg tea.Msg) (tea.Model, tea.Cmd) {
	if events := m.clarifyBridge.Events(); events != nil {
		select {
		case req := <-events:
			m.armClarifyPrompt(req, false)
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
		if m.editor != nil {
			m.editor.SetLayoutWidth(msg.Width)
		}
		if m.pager != nil {
			m.pager.Resize(msg.Width, msg.Height)
		}
		// Hybrid: commit the startup card to scrollback once, now that the width
		// is known, so it persists at the top of history (like Codex) instead of
		// vanishing when the first message scrolls the active region.
		if !m.startupCommitted && msg.Width > 0 {
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
		if m.flushLiveStreamPending() {
		}
		return m, m.scheduleStreamFlush()

	case MsgAgentActivity:
		m.activityText = strings.TrimSpace(msg.Content)
		m.thinking = true
		if m.thinkingStart.IsZero() {
			m.thinkingStart = time.Now()
		}
		return m, spinnerCmd

	case tea.KeyMsg:
		if m.onUserInput != nil {
			m.onUserInput() // presence honesty: every keystroke counts as "the person is here" (input_activity.go)
		}
		if m.pager != nil {
			closed, cmd := m.pager.Update(msg)
			if closed {
				m.pager = nil
			}
			return m, cmd
		}
		return m.handleKey(msg)

	case MsgStream:
		msg.Content = textutil.CleanUTF8(msg.Content)
		m.thinking = false
		m.activityText = ""
		if committed := m.streamController.Push(msg.Content); committed != "" {
			m.commitLiveStream(committed)
		}
		return m, m.scheduleStreamFlush()

	case MsgAgentDone:
		m.exitPromptActive = false
		// The run is over; any unanswered approval row is expired by the daemon
		// once its waiter is gone, so drop stale approval UI instead of letting a
		// panel answer into the void.
		m.clearApprovalFlow()
		msg.Response = textutil.CleanUTF8(msg.Response)
		m.thinking = false
		m.activityText = ""
		m.activePlanJSON = ""
		m.toolExecuting = ""
		m.discardOpenToolMessages()
		m.steerCh = nil // run finished; stop accepting mid-turn guidance for it
		m.clarifyMode = false
		m.clarifyGateway = false
		m.clarifyChoices = nil
		m.clarifyReq = tools.ClarifyRequest{}
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
		m.toolExecuting = ""
		m.activePlanJSON = ""
		m.discardOpenToolMessages()
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

	case MsgClarifyRequest:
		m.armClarifyPrompt(tools.ClarifyRequest{Question: msg.Question, Choices: msg.Choices}, true)
		return m, nil

	case MsgClarifyAnswerResult:
		if msg.Err != nil {
			m.addErrorMessage(fmt.Sprintf("Could not send clarify answer: %v", msg.Err))
			m.runStatus = "error"
			m.statusMsg = "Clarify answer was not accepted."
		} else {
			m.statusMsg = "Answer sent. The task is continuing."
			m.runStatus = "working"
		}
		return m, tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return MsgClearStatus{} })

	case MsgToolStart:
		if isTerminalRunStatus(m.runStatus) || m.toolMessageExists(msg.ToolCallID) {
			return m, spinnerCmd
		}
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
		return m, spinnerCmd

	case MsgToolDone:
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		msg.Result = textutil.CleanUTF8(msg.Result)
		idx := m.findActiveToolMessageIndex(msg.ToolCallID, msg.ToolName)
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
		return m, spinnerCmd

	case MsgPlanUpdated:
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		if content := strings.TrimSpace(textutil.CleanUTF8(msg.Content)); content != "" {
			m.activePlanJSON = content
		}
		return m, spinnerCmd

	case MsgToolOutput:
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		m.thinking = false
		m.activityText = ""
		m.appendToolOutput(msg.ToolCallID, msg.ToolName, msg.Content)
		return m, spinnerCmd

	case MsgToolHeartbeat:
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		idx := m.findActiveToolMessageIndex(msg.ToolCallID, msg.ToolName)
		if idx >= 0 {
			if msg.ToolName != "" {
				m.toolExecuting = msg.ToolName
			}
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
		m.statusMsg = msg.Content
		m.addMessage("system", msg.Content)
		return m, nil

	case MsgClearStatus:
		m.statusMsg = ""
		return m, nil

	case MsgTokens:
		// Live cumulative usage snapshot for the active run (token.updated).
		// Only runTokens ticks here; totalTokens stays untouched so the final
		// MsgAgentDone usage remains the single authoritative session
		// increment (no double counting).
		if msg.Run > 0 {
			m.runTokens = msg.Run
		}
		return m, nil

	case MsgWorkspaceSwitched:
		// A successful /workspace switch: pin the session override, then render
		// the gateway's reply through the normal done path so the transcript
		// looks identical to a plain control passthrough.
		m.workspaceOverrideID = msg.ID
		m.workspaceOverrideName = msg.Name
		m.workspaceOverridePath = msg.Path
		return m.Update(MsgAgentDone{Response: msg.Reply})

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
			m.activePlanJSON = ""
			return m, m.clearHybridScreen()
		case "enter":
			// Deny follow-up (approval panel "No"): Enter resolves it — bare Enter
			// is a plain deny, typed text is deny + guidance. Checked before the
			// empty-input early return so bare Enter works.
			if m.approvalDenyFollowup {
				return m, m.finishApprovalDeny(m.editor.ExpandValue())
			}
			// Slash-command popup is open (a partial like "/m" with a highlighted
			// match): Enter accepts the highlighted command and submits it in one
			// press, so the user never has to type the command in full. Safe for
			// commands with args — the popup closes as soon as a space is typed
			// (matchingCommands), so "/task 3 rename foo" is never clobbered.
			if m.editor.SuggestionsVisible() {
				m.editor.AcceptSuggestion()
			}
			// Shift+Enter / Ctrl+J already handled above via KeyCtrlJ.
			// Here plain Enter submits
			// Use ExpandValue() to replace paste placeholders with actual content.
			input := m.editor.ExpandValue()
			if input == "" {
				return m, nil
			}
			// Record the EXPANDED input, never displayInput: paste placeholders
			// are unrecoverable after editor.Reset() clears the snippet buffer
			// (recordInputHistory also skips secure and oversized inputs).
			m.recordInputHistory(input)

			if m.clarifyMode {
				response := m.resolveClarifyResponse(input)
				m.addMessage("user", response)
				m.editor.Reset()
				if m.clarifyGateway {
					m.clarifyMode = false
					m.clarifyGateway = false
					m.clarifyChoices = nil
					m.clarifyReq = tools.ClarifyRequest{}
					m.thinking = true
					m.runStatus = "working"
					m.thinkingStart = time.Now()
					m.thinkingDots = 0
					m.activityText = "Waiting for the task to continue"
					return m, tea.Batch(m.answerClarifyViaGateway(response), m.spinner.Tick, workingTick())
				}
				m.clarifyBridge.Submit(m.clarifyReq, response)
				m.clarifyMode = false
				m.clarifyGateway = false
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
			m.activePlanJSON = ""
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
