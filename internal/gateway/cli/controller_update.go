package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/gateway/command"
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

	// In-session update announcement: consumed only when idle (update_notice.go).
	m.maybeAnnounceUpdate()

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
		if m.thinking || m.toolExecuting != "" || m.daemonRunActive {
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
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		m.activityText = strings.TrimSpace(msg.Content)
		if !m.passiveDaemonEvent(msg.Event) {
			m.thinking = true
			if m.thinkingStart.IsZero() {
				m.thinkingStart = time.Now()
			}
		}
		return m, spinnerCmd

	case tea.KeyMsg:
		// Same signal, different purpose: the approval panel must not arm while
		// the person is mid-keystroke (approvalTypingIdleDelay).
		m.noteInputActivity(time.Now())
		if m.pager != nil {
			closed, cmd := m.pager.Update(msg)
			if closed {
				m.pager = nil
			}
			return m, cmd
		}
		return m.handleKey(msg)

	case MsgStream:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		msg.Content = textutil.CleanUTF8(msg.Content)
		m.thinking = false
		m.activityText = ""
		if committed := m.streamController.Push(msg.Content); committed != "" {
			m.commitLiveStream(committed)
		}
		return m, m.scheduleStreamFlush()

	case MsgAgentDone:
		m.exitPromptActive = false
		if queuedTurn(msg.Turn) {
			msg.Response = textutil.CleanUTF8(msg.Response)
			m.localRequestActive = false
			m.localRequestInput = ""
			m.thinking = false
			m.activityText = ""
			m.steerCh = nil
			m.cancelFn = nil
			if input := strings.TrimSpace(msg.Input); input != "" {
				m.queuedInputs = append(m.queuedInputs, input)
				m.queuedCount++
			}
			m.finalizeLiveStream(msg.Response)
			if m.daemonRunActive {
				m.runStatus = "working"
			} else {
				m.runStatus = "queued"
			}
			return m, spinnerCmd
		}
		newerDaemonRun := m.daemonRunActive && msg.Turn != nil &&
			strings.TrimSpace(msg.Turn.RunID) != "" &&
			strings.TrimSpace(msg.Turn.RunID) != m.daemonRunID
		// Live waiters die with the run, but a parked approval deliberately stays
		// answerable and starts a continuation. Preserve those panels.
		if !newerDaemonRun {
			m.settleApprovalFlowAtRunEnd()
		}
		msg.Response = textutil.CleanUTF8(msg.Response)
		m.thinking = false
		m.activityText = ""
		if !newerDaemonRun {
			m.activePlanJSON = ""
			m.toolExecuting = ""
			m.finalizeOpenToolMessages("Completion was not observed before the run ended.")
		}
		m.steerCh = nil // run finished; stop accepting mid-turn guidance for it
		m.cancelFn = nil
		m.localRequestActive = false
		m.localRequestInput = ""
		if !newerDaemonRun {
			m.daemonRunAwaitingDone = false
			m.clarifyMode = false
			m.clarifyGateway = false
			m.clarifyChoices = nil
			m.clarifyReq = tools.ClarifyRequest{}
		}
		// The mid-turn guidance notice is stale once the run ends — clear it.
		if strings.HasPrefix(m.statusMsg, "Sent to the running task") || strings.Contains(m.statusMsg, "Guidance queue") {
			m.clearStatusNotice()
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
		if newerDaemonRun {
			m.runStatus = "working"
		} else if m.queuedCount > 0 {
			m.runStatus = "queued"
		}
		return m, spinnerCmd

	case MsgDaemonRunStarted:
		if !m.acceptEvent(msg.Event) {
			return m, spinnerCmd
		}
		localMatch := m.localRequestActive && sameQueuedInput(m.localRequestInput, msg.Input)
		queuedMatch := false
		if !localMatch {
			queuedMatch = m.consumeQueuedInput(msg.Input)
		}
		m.daemonRunActive = true
		m.daemonRunID = msg.RunID
		m.daemonRunStarted = msg.Started
		if m.daemonRunStarted.IsZero() {
			m.daemonRunStarted = time.Now()
		}
		m.daemonRunAwaitingDone = localMatch
		m.runStatus = "working"
		m.runTokens = 0
		m.activePlanJSON = ""
		if !localMatch {
			m.thinking = false
			m.activityText = ""
			m.toolExecuting = ""
			m.finalizeOpenToolMessages("Completion was not observed before the next run started.")
		}
		watchID := strings.TrimSpace(msg.WatchID)
		// The daemon started this run on the person's behalf, so it reports its
		// result and not its process (markBackgroundRun). A watcher also opens
		// with a notice, because it continues a boundary the person already
		// saw; a bare background run stays silent until it has something to
		// report — the status bar already shows the daemon is busy.
		if watchID != "" || strings.TrimSpace(msg.Origin) != "" {
			m.markBackgroundRun(msg.RunID, watchID, msg.Origin)
		}
		if watchID != "" {
			taskStatus := strings.TrimSpace(msg.TaskStatus)
			if taskStatus == "" {
				taskStatus = "running"
			}
			m.addMessage("notice", watcherStatusNotice(watchID, "finalizing", taskStatus))
		} else if queuedMatch && strings.TrimSpace(msg.Origin) == "" {
			title := strings.TrimSpace(msg.Input)
			if title == "" {
				title = "queued task"
			}
			m.addMessage("notice", "Queued task started: "+title)
		}
		return m, workingTick()

	case MsgDaemonRunFinished:
		if !m.acceptEvent(msg.Event) {
			return m, spinnerCmd
		}
		if !m.daemonRunActive {
			return m, spinnerCmd
		}
		if m.daemonRunID != "" && msg.RunID != "" && msg.RunID != m.daemonRunID {
			return m, spinnerCmd
		}
		awaitingSynchronousDone := m.daemonRunAwaitingDone
		m.daemonRunActive = false
		m.daemonRunID = ""
		m.daemonRunStarted = time.Time{}
		m.daemonRunAwaitingDone = false
		if awaitingSynchronousDone {
			return m, spinnerCmd
		}
		if backgroundWatchID, backgroundOrigin, backgroundRun := m.finishedBackgroundRun(msg.RunID); backgroundRun {
			// Nothing of this run entered the transcript, the live stream, or
			// the tool cells, so none of the foreground cleanup below applies —
			// running it would discard state that belongs to this terminal.
			// The recorded outcome is the whole visible result.
			if m.queuedCount > 0 {
				m.runStatus = "queued"
			} else {
				m.runStatus = uiStatusForDaemonOutcome(msg.Status)
			}
			m.addMessage("notice", backgroundResultNotice(backgroundWatchID, backgroundOrigin, msg.Status, msg.Summary))
			return m, spinnerCmd
		}
		m.thinking = false
		m.activityText = ""
		m.toolExecuting = ""
		m.activePlanJSON = ""
		m.finalizeOpenToolMessages("Completion was not observed before the run ended.")
		m.flushLiveStreamPending()
		if strings.TrimSpace(m.liveStreamContent) != "" {
			m.finalizeLiveStream("")
		} else {
			m.finalizeLiveStream(msg.Summary)
		}
		if m.queuedCount > 0 {
			m.runStatus = "queued"
		} else {
			m.runStatus = uiStatusForDaemonOutcome(msg.Status)
		}
		return m, spinnerCmd

	case MsgAttachedRunDone:
		// The watched (re-attached) daemon run ended. Unlike MsgAgentDone there
		// is no synchronous answer to finalize — the conversation reply follows
		// the run's ORIGIN endpoint (docs/identity-continuity.md); this watcher
		// only reports that the observation ended and the recorded outcome.
		if !m.watchingRun {
			return m, spinnerCmd
		}
		if msg.RunID != "" && m.watchedRunID != "" && msg.RunID != m.watchedRunID {
			return m, spinnerCmd
		}
		wasWatching := m.watchingRun
		m.finalizeLiveStream("")
		m.watchingRun = false
		m.watchedRunID = ""
		m.watchedTaskTitle = ""
		m.watchCancel = nil
		m.toolExecuting = ""
		m.activePlanJSON = ""
		if msg.Cancelled {
			// This terminal detached from a still-running daemon task. Its durable
			// tool ledger remains authoritative; transient spectator rows need not
			// be committed as failures.
			m.discardOpenToolMessages()
		} else {
			m.finalizeOpenToolMessages("Completion was not observed before the watched run ended.")
		}
		if strings.HasPrefix(m.statusMsg, "Watching ") {
			m.clearStatusNotice()
		}
		if msg.Cancelled {
			// The user detached (ctrl+c during watch) — the run keeps running on
			// the daemon; just stop reporting. No "finished" line.
			if wasWatching {
				m.setStatusNotice(noticeInfo, "Detached — the task keeps running in the background.")
			}
			return m, spinnerCmd
		}
		m.runStatus = "done"
		if summary := strings.TrimSpace(msg.Summary); summary != "" {
			m.addMessage("notice", "The running task finished: "+summary)
		} else {
			m.addMessage("notice", "The running task finished. Use /status for details.")
		}
		return m, spinnerCmd

	case MsgApprovalRequest:
		if m.hasApprovalRequest(msg.ID) {
			return m, nil
		}
		// The daemon is blocked waiting for approval. Arm the interactive panel
		// (active region); if one is already up, queue FIFO and re-arm after the
		// current decision. No redundant text notice — the panel is the prompt.
		if m.approvalFlowActive() {
			m.approvalQueue = append(m.approvalQueue, msg)
			return m, nil
		}
		// Typing-idle guard: a panel that arms mid-keystroke can read the next
		// letter as a decision. Hold it (FIFO with anything already held) until
		// input settles; the daemon-side request is durable meanwhile.
		if len(m.delayedApprovals) > 0 {
			return m, m.holdApprovalRequest(msg, approvalTypingIdleDelay)
		}
		if wait := m.approvalDelayRemaining(time.Now()); wait > 0 {
			return m, m.holdApprovalRequest(msg, wait)
		}
		m.armApprovalPrompt(msg)
		return m, nil

	case MsgApprovalDelayElapsed:
		return m, m.releaseDelayedApprovals(time.Now())

	case MsgApprovalResolved:
		if !m.acceptEvent(msg.Event) {
			return m, nil
		}
		return m, m.resolveApprovalElsewhere(msg)

	case MsgApprovalParked:
		if !m.acceptEvent(msg.Event) {
			return m, nil
		}
		m.markApprovalParked(msg.ID)
		return m, nil

	case MsgClarifyRequest:
		m.armClarifyPrompt(tools.ClarifyRequest{Question: msg.Question, Choices: msg.Choices}, true)
		return m, nil

	case MsgClarifyAnswerResult:
		if msg.Err != nil {
			m.addErrorMessage(fmt.Sprintf("Could not send clarify answer: %v", msg.Err))
			m.runStatus = "error"
			noticeID := m.setStatusNotice(noticeWarning, "Clarify answer was not accepted.")
			return m, clearStatusNoticeAfter(noticeID, 3*time.Second)
		} else {
			noticeID := m.setStatusNotice(noticeGuidance, "Answer sent. The task is continuing.")
			m.runStatus = "working"
			return m, clearStatusNoticeAfter(noticeID, 3*time.Second)
		}

	case MsgToolStart:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		// Anonymous mutable rows cannot be completed deterministically and would
		// remain in the redraw region forever. Producers must provide identity;
		// legacy compatibility belongs at the event adapter boundary.
		if strings.TrimSpace(msg.ToolCallID) == "" {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) || m.toolMessageIndex(msg.ToolCallID, msg.Event.RunID) >= 0 {
			return m, spinnerCmd
		}
		m.finalizeLiveStream("")
		m.thinking = false
		m.activityText = ""
		if !m.passiveDaemonEvent(msg.Event) {
			m.toolExecuting = msg.ToolName
		}
		m.addMessage("tool", "")
		last := &m.messages[len(m.messages)-1]
		last.ToolName = msg.ToolName
		last.ToolCallID = msg.ToolCallID
		last.RunID = msg.Event.RunID
		last.ToolArgs = msg.Args
		last.IsRunning = true
		return m, spinnerCmd

	case MsgToolDone:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		msg.Result = textutil.CleanUTF8(msg.Result)
		idx := m.findActiveToolMessageIndex(msg.ToolCallID, msg.ToolName, msg.Event.RunID)
		if idx >= 0 {
			last := &m.messages[idx]
			if strings.TrimSpace(msg.ToolName) != "" {
				last.ToolName = msg.ToolName
			}
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
		} else if strings.TrimSpace(msg.ToolCallID) != "" &&
			m.toolMessageIndex(msg.ToolCallID, msg.Event.RunID) < 0 &&
			m.currentToolEvent(msg.Event) {
			// A completion for this run that has no tracked start is not discarded
			// and never guessed onto another active call. Give it a standalone,
			// already-completed history cell: the explicit orphan destination.
			toolName := strings.TrimSpace(msg.ToolName)
			if toolName == "" {
				toolName = "tool"
			}
			m.messages = append(m.messages, ChatMessage{
				Role:       "tool",
				ToolName:   toolName,
				ToolCallID: msg.ToolCallID,
				RunID:      msg.Event.RunID,
				Content:    msg.Result,
				Duration:   msg.Duration,
				IsError:    msg.Err != nil,
				Timestamp:  time.Now(),
			})
			orphan := &m.messages[len(m.messages)-1]
			if msg.Err != nil {
				orphan.Content = fmt.Sprintf("%s error: %v", toolName, msg.Err)
			}
			m.commit(orphan)
		}
		if !m.anyToolRunning() && !m.passiveDaemonEvent(msg.Event) {
			m.toolExecuting = ""
		}
		return m, spinnerCmd

	case MsgPlanUpdated:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		if content := strings.TrimSpace(textutil.CleanUTF8(msg.Content)); content != "" {
			m.activePlanJSON = content
		}
		return m, spinnerCmd

	case MsgToolOutput:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		m.thinking = false
		m.activityText = ""
		m.appendToolOutput(msg.ToolCallID, msg.ToolName, msg.Event.RunID, msg.Content)
		return m, spinnerCmd

	case MsgToolHeartbeat:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		idx := m.findActiveToolMessageIndex(msg.ToolCallID, msg.ToolName, msg.Event.RunID)
		if idx >= 0 {
			if msg.ToolName != "" && !m.passiveDaemonEvent(msg.Event) {
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
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		m.setStatusNotice(noticeInfo, msg.Content)
		m.addMessage("system", msg.Content)
		return m, nil

	case MsgWatcherCompleted:
		if !m.acceptEvent(msg.Event) {
			return m, spinnerCmd
		}
		m.addMessage("notice", watcherStatusNotice(msg.WatchID, msg.Status, msg.TaskStatus))
		return m, nil

	case MsgClearStatus:
		if msg.NoticeID == 0 || msg.NoticeID == m.statusNoticeID {
			m.clearStatusNotice()
		}
		return m, nil

	case MsgTokens:
		if !m.acceptEvent(msg.Event) {
			return m, spinnerCmd
		}
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
	// An active approval panel captures every key. Esc and Ctrl+C explicitly
	// reject the current request; stray keys must not leak into the composer.
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
			if m.localRequestActive {
				if m.exitPromptActive {
					return m, m.quitNow()
				}
				m.exitPromptActive = true
				m.addMessage("assistant", "A task is still running. Choose:\n  b — quit and leave it running in the background (the result will be pushed to your bound IM)\n  c — cancel the task and stay\n  esc — keep watching")
				return m, nil
			}
			// Priority 3: quit
			return m, m.quitNow()
		case "ctrl+l":
			m.messages = []ChatMessage{}
			m.clearLiveStream()
			m.activePlanJSON = ""
			return m, m.clearHybridScreen()
		case "enter":
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
			// The display form (with compact [[ paste:N ]] / [[ image:N ]] tokens)
			// is what the transcript echoes; the expanded form is what runs.
			display := strings.TrimSpace(m.editor.Value())
			input := m.editor.ExpandValue()
			if input == "" {
				return m, nil
			}
			// A placeholder that survived expansion means its payload can no
			// longer be recovered (an edited token, or one restored from an older
			// client). Refuse locally and KEEP the composer: resetting it here is
			// what previously turned the daemon's "paste it again" into a dead
			// end, because the snippet buffer was already gone.
			if stranded := m.editor.UnresolvedToken(); stranded != "" {
				m.addMessage("notice", "This paste placeholder lost its content and was not sent: "+stranded+"\nThe text is still in your clipboard — delete the placeholder and paste it again.")
				return m, nil
			}
			if display == "" {
				display = input
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

			// An armed /resume picker reads the next bare number as a menu pick.
			// Anything else disarms it, so a digit-leading sentence typed later
			// still reaches the agent as ordinary text.
			if m.resumePickerArmed {
				m.resumePickerArmed = false
				if n := bareListNumber(input); n != "" {
					input = "/resume " + n
					display = input
				}
			}

			// Only command-SHAPED tokens enter command handling ("/help",
			// "/paste-image"). A "/"-leading absolute path ("/mnt/c/pic.png …")
			// is ordinary agent-first message text (shared predicate with the
			// gateway reject gate — observed live as a bogus Unknown command).
			if command.LooksLikeCommand(input) {
				return m, m.handleCommand(input)
			}
			// Mid-turn steering: if a run is active, inject this as guidance into
			// the running turn instead of starting a competing run (which the
			// busy-guard would reject) or overwriting the in-flight cancelFn.
			if m.localRequestActive {
				return m, m.injectMidRunGuidance(input)
			}
			m.detachWatchedRunForNewTurn()
			// Echo the compact display form: a 200-line paste or an attached
			// image shows as its token, not as the expanded payload.
			m.addMessage("user", display)
			m.editor.Reset()
			m.activePlanJSON = ""
			m.steerCh = make(chan string, 16)
			m.localRequestActive = true
			m.localRequestInput = input
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
