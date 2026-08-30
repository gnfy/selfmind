package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
	"selfmind/internal/ui/components"

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

	var spinnerCmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.common.Width, m.common.Height = msg.Width, msg.Height
		if m.editor != nil {
			m.editor.SetLayout(msg.Width, msg.Height)
		}
		if m.pager != nil {
			m.pager.Resize(msg.Width, msg.Height)
		}
		if m.modelManager != nil {
			m.modelManager.Resize(msg.Width, msg.Height)
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
		if !m.waitingForModel {
			return m, nil
		}
		m.spinner, spinnerCmd = m.spinner.Update(msg)
		return m, spinnerCmd

	case MsgWorkingTick:
		if m.thinking || m.toolExecuting != "" || (m.daemonRunActive && !m.backgroundDaemonRunActive()) {
			return m, workingTick()
		}
		return m, nil

	case MsgCursorBlinkTick:
		m.cursorVisible = !m.cursorVisible
		if m.editor != nil {
			m.editor.SetCursorVisible(m.cursorVisible)
		}
		return m, cursorBlinkTick()

	case MsgAgentActivity:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if strings.EqualFold(strings.TrimSpace(msg.Phase), modelWaitPhase) && !m.passiveDaemonEvent(msg.Event) {
			return m, m.startModelWait(msg.Content)
		}
		m.stopModelWait()
		m.activityText = ""
		return m, spinnerCmd

	case MsgBackgroundNotice:
		if content := strings.TrimSpace(textutil.CleanUTF8(msg.Content)); content != "" {
			kind := noticeWarning
			if msg.Success {
				kind = noticeSuccess
			}
			m.addNotice(kind, content)
		}
		return m, nil

	case MsgModelValidationDone:
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		if m.modelManager == nil {
			return m, nil
		}
		if msg.Err != nil {
			m.modelManager.SetRouteValidation(msg.Route, false, msg.Err.Error(), "")
			return m, nil
		}
		ok := len(msg.Response.Probes) > 0
		var failures []string
		for _, probe := range msg.Response.Probes {
			if probe.OK {
				continue
			}
			ok = false
			failure := strings.TrimSpace(probe.Error)
			if failure == "" {
				failure = "probe failed"
			}
			failures = append(failures, string(probe.Route)+": "+failure)
		}
		message := strings.Join(failures, "; ")
		if !ok && message == "" {
			message = "validation returned no evidence"
		}
		m.modelManager.SetRouteValidation(msg.Route, ok, message, msg.Response.CredentialStage)
		return m, nil

	case MsgModelChangeDone:
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		if msg.Err != nil {
			m.addErrorMessage("Model change failed: " + msg.Err.Error())
			return m, nil
		}
		if msg.Response.Change == nil {
			m.addErrorMessage("Model change failed: the daemon returned no transaction receipt.")
			return m, nil
		}
		m.modelManager = nil
		change := msg.Response.Change
		m.modelManagerStatus.ConfiguredPrimary = selectionDisplay(change.Candidate.Primary)
		m.modelManagerStatus.ConfiguredBackground = selectionDisplay(change.Candidate.Auxiliary)
		m.modelManagerStatus.PrimaryProvider = change.Candidate.Primary.Provider
		m.modelManagerStatus.PrimaryModel = change.Candidate.Primary.Model
		m.modelManagerStatus.PrimaryReasoning = change.Candidate.Primary.Reasoning
		m.modelManagerStatus.PrimaryServiceTier = change.Candidate.Primary.ServiceTier
		m.modelManagerStatus.BackgroundProvider = change.Candidate.Auxiliary.Provider
		m.modelManagerStatus.BackgroundModel = change.Candidate.Auxiliary.Model
		m.modelManagerStatus.BackgroundReasoning = change.Candidate.Auxiliary.Reasoning
		m.modelManagerStatus.BackgroundServiceTier = change.Candidate.Auxiliary.ServiceTier
		m.modelManagerStatus.RoleOverrides = make(map[string]components.ModelManagerSubmission)
		for _, route := range modelchange.ManagedRoleRoutes() {
			selection := modelchange.SelectionForRoute(change.Candidate, route)
			if strings.TrimSpace(selection.Provider) == "" && strings.TrimSpace(selection.Model) == "" {
				continue
			}
			m.modelManagerStatus.RoleOverrides[string(route)] = components.ModelManagerSubmission{
				Route: string(route), Provider: selection.Provider, Model: selection.Model,
				Reasoning: selection.Reasoning, ServiceTier: selection.ServiceTier,
			}
		}
		m.modelManagerStatus.Pending = fmt.Sprintf("%s (%s)", change.ID, change.Status)
		m.modelChangeID = change.ID
		m.modelChangePhase = change.Status
		m.modelChangePhaseAt = change.PhaseStartedAt
		for _, notice := range msg.Response.Notices {
			m.addMessage("notice", "Model change notice: "+notice)
		}
		if msg.Response.RestartScheduled {
			m.addMessage("notice", fmt.Sprintf("Model change %s validated and saved. Running remains %s until the safe restart is healthy.", change.ID, m.displayModelName()))
			if m.modelManagerOnly {
				return m, m.quitNow()
			}
			return m, m.observeModelChange(false, 100*time.Millisecond)
		} else {
			m.addMessage("notice", fmt.Sprintf("Model change %s validated and saved. Run `selfmind gateway restart --drain` to apply it.", change.ID))
		}
		if m.modelManagerOnly {
			return m, m.quitNow()
		}
		return m, nil

	case MsgModelChangeObserved:
		if msg.Err != nil {
			if msg.OpenManager {
				m.addErrorMessage("Could not inspect model change state: " + msg.Err.Error())
				return m, nil
			}
			// A planned restart may briefly make HTTP unavailable, but the local
			// observer normally still has the durable transaction file. A transient
			// observation failure is retried without leaking connection errors.
			return m, m.observeModelChange(false, 250*time.Millisecond)
		}
		observedID := m.modelChangeID
		status := msg.Observation.Status
		m.applyModelStatus(status)
		if status.Pending != nil {
			m.modelGatewayOffline = !msg.Observation.GatewayReachable &&
				(status.Pending.Status == modelchange.StatusDraining || status.Pending.Status == modelchange.StatusRestarting || status.Pending.Status == modelchange.StatusStarting)
			if !m.modelChangeSlowWarned && !status.Pending.CreatedAt.IsZero() && time.Since(status.Pending.CreatedAt) >= 30*time.Second {
				m.modelChangeSlowWarned = true
				m.addMessage("notice", fmt.Sprintf("Model change %s is taking longer than 30 seconds. SelfMind is still waiting for a safe run boundary or gateway health.", status.Pending.ID))
			}
			if status.Pending.Status == modelchange.StatusRecoveryRequired {
				m.addErrorMessage(fmt.Sprintf("Model change %s requires recovery: %s", status.Pending.ID, status.Pending.Failure))
			} else {
				if msg.OpenManager {
					m.modelManager = components.NewModelManagerWithTheme(m.modelManagerStatus, m.modelManagerRoutes, m.width, m.height, m.common.Theme)
				}
				return m, m.observeModelChange(false, 250*time.Millisecond)
			}
		} else if observedID != "" {
			for i := len(status.History) - 1; i >= 0; i-- {
				change := status.History[i]
				if change.ID != observedID {
					continue
				}
				switch change.Status {
				case modelchange.StatusApplied:
					m.addMessage("notice", fmt.Sprintf("Model change %s applied. Running is now %s.", change.ID, m.displayModelName()))
				case modelchange.StatusRolledBack:
					m.addErrorMessage(fmt.Sprintf("Model change %s was rolled back: %s", change.ID, change.Failure))
				case modelchange.StatusConflict, modelchange.StatusFailed, modelchange.StatusCancelled, modelchange.StatusSuperseded:
					m.addErrorMessage(fmt.Sprintf("Model change %s ended as %s: %s", change.ID, change.Status, change.Failure))
				}
				break
			}
		}
		if msg.OpenManager {
			m.modelManager = components.NewModelManagerWithTheme(m.modelManagerStatus, m.modelManagerRoutes, m.width, m.height, m.common.Theme)
		}
		return m, nil

	case MsgModelRecoveryDone:
		if msg.Err != nil {
			m.addErrorMessage("Model recovery failed: " + msg.Err.Error())
			return m, nil
		}
		m.addMessage("notice", "Model recovery action accepted: "+msg.Action+".")
		if m.modelManagerOnly {
			return m, m.quitNow()
		}
		return m, m.observeModelChange(false, 100*time.Millisecond)

	case MsgModelManagerOpen:
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		if msg.Err != nil {
			m.addErrorMessage("Could not load model routes: " + msg.Err.Error())
			return m, nil
		}
		if msg.Response.Status == nil {
			m.addErrorMessage("Could not load model routes: the daemon returned no status.")
			return m, nil
		}
		if msg.Response.ProtocolVersion < api.ModelControlProtocolVersion {
			m.addErrorMessage("The running SelfMind service is too old for Provider connections. Run `selfmind gateway restart --drain`, then reopen Model Manager.")
			return m, nil
		}
		m.modelManagerStatus = modelManagerStatusFrom(*msg.Response.Status)
		m.modelManager = components.NewModelManagerWithTheme(m.modelManagerStatus, m.modelManagerRoutes, m.width, m.height, m.common.Theme)
		return m, nil

	case tea.KeyMsg:
		// Same signal, different purpose: the approval panel must not arm while
		// the person is mid-keystroke (approvalTypingIdleDelay).
		m.noteInputActivity(time.Now())
		if m.approvalPrompt != nil {
			return m.handleKey(msg)
		}
		if m.modelManager != nil {
			action := m.modelManager.Update(msg)
			if action.RecoveryAction != "" {
				m.modelManager = nil
				return m, m.recoverModelChange(action.RecoveryAction)
			}
			if action.ValidationRoute != "" {
				if len(action.Draft) == 0 {
					m.modelManager.SetRouteValidation(action.ValidationRoute, true, "", "")
					return m, nil
				}
				m.thinking = true
				m.thinkingStart = time.Now()
				m.activityText = "Validating model selection"
				return m, m.validateModelManager(action.ValidationRoute, action.Draft, action.ProviderDraft, m.modelManager.CredentialStage())
			}
			if action.Closed && (len(action.Draft) > 0 || len(action.ProviderDraft) > 0) {
				m.thinking = true
				m.thinkingStart = time.Now()
				m.activityText = "Applying model changes"
				return m, m.submitModelManager(action.Draft, action.ProviderDraft, m.modelManager.CredentialStage())
			}
			if action.Closed {
				m.modelManager = nil
				if m.modelManagerOnly {
					return m, m.quitNow()
				}
			}
			return m, nil
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
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		msg.Content = textutil.CleanUTF8(msg.Content)
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		m.applyProcessEffects(m.processState().Update(processEvent{
			kind: processStreamDelta, content: msg.Content, phase: msg.Phase,
		}))
		return m, spinnerCmd

	case MsgSkillCompletionLoaded:
		// A refresh failure leaves the previous inventory in place: completion is
		// an affordance, and losing it silently is better than replacing it with
		// an empty popup or an error the person did not ask for.
		if msg.Err == nil {
			m.skillCompletion = msg.Candidates
		}
		return m, nil
	case MsgSkillInvocationResolved:
		return m, m.finishSkillInvocationResolution(msg)

	case MsgAgentDone:
		m.stopModelWait()
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
			m.finalizeLiveStream(msg.Response, llm.AssistantPhaseFinalAnswer)
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
			m.finalizeLiveStream(msg.Response, llm.AssistantPhaseFinalAnswer)
			m.addErrorMessage(fmt.Sprintf("Error: %v", msg.Err))
		} else if m.finalizeLiveStream(msg.Response, llm.AssistantPhaseFinalAnswer) {
			m.runStatus = "done"
			if strings.TrimSpace(msg.Input) != "" && !strings.HasPrefix(strings.TrimSpace(msg.Input), "/") {
				m.completeFirstOnboardingTask()
			}
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
			m.stopModelWait()
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
		if m.backgroundDaemonRunActive() {
			return m, spinnerCmd
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
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		m.toolExecuting = ""
		m.activePlanJSON = ""
		m.finalizeOpenToolMessages("Completion was not observed before the run ended.")
		if m.processState().HasStreamContent() {
			m.finalizeLiveStream("", llm.AssistantPhaseFinalAnswer)
		} else {
			m.finalizeLiveStream(msg.Summary, llm.AssistantPhaseFinalAnswer)
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
		m.finalizeLiveStream("", llm.AssistantPhaseFinalAnswer)
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
		m.stopModelWait()
		if m.hasApprovalRequest(msg.ID) {
			return m, nil
		}
		// The daemon is blocked waiting for approval. Arm the inline panel;
		// if one is already up, queue FIFO and re-arm after the
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
		m.stopModelWait()
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
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		effects := m.processState().Update(processEvent{
			kind: processToolStarted, toolName: msg.ToolName, toolCallID: msg.ToolCallID,
			toolArgs: msg.Args, runID: msg.Event.RunID,
		})
		m.applyProcessEffects(effects)
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		if !m.passiveDaemonEvent(msg.Event) {
			m.toolExecuting = msg.ToolName
		}
		return m, spinnerCmd

	case MsgToolDone:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		msg.Result = textutil.CleanUTF8(msg.Result)
		m.applyProcessEffects(m.processState().Update(processEvent{
			kind: processToolCompleted, toolName: msg.ToolName, toolCallID: msg.ToolCallID,
			runID: msg.Event.RunID, result: msg.Result, err: msg.Err, duration: msg.Duration,
			allowOrphan: m.currentToolEvent(msg.Event),
		}))
		if !m.processState().HasRunningTools() && !m.passiveDaemonEvent(msg.Event) {
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
		m.stopModelWait()
		m.thinking = false
		m.activityText = ""
		m.applyProcessEffects(m.processState().Update(processEvent{
			kind: processToolOutput, toolName: msg.ToolName, toolCallID: msg.ToolCallID,
			runID: msg.Event.RunID, content: textutil.CleanUTF8(msg.Content),
		}))
		return m, spinnerCmd

	case MsgToolHeartbeat:
		if !m.acceptEvent(msg.Event) || m.backgroundRunEvent(msg.Event) {
			return m, spinnerCmd
		}
		if isTerminalRunStatus(m.runStatus) {
			return m, spinnerCmd
		}
		if msg.ToolName != "" && !m.passiveDaemonEvent(msg.Event) {
			m.toolExecuting = msg.ToolName
		}
		m.applyProcessEffects(m.processState().Update(processEvent{
			kind: processToolHeartbeat, toolName: msg.ToolName, toolCallID: msg.ToolCallID,
			runID: msg.Event.RunID, detail: textutil.CleanUTF8(msg.Content),
		}))
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

	// Composer-local navigation and completion are arbitrated inside Editor so
	// recalled slash commands cannot reopen a popup that steals history arrows.
	// Ctrl+J inserts a newline (multi-line input); plain Enter submits.
	case tea.KeyCtrlJ:
		result := m.editor.HandleKey(msg)
		return m, result.Cmd
	case tea.KeyUp, tea.KeyDown, tea.KeyTab:
		result := m.editor.HandleKey(msg)
		return m, result.Cmd

	default:
		if m.exitPromptActive {
			if cmd, handled := m.handleExitPromptKey(msg.String()); handled {
				return m, cmd
			}
		}
		switch msg.String() {
		case "esc":
			if result := m.editor.HandleKey(msg); result.Handled() {
				return m, result.Cmd
			}
			return m, nil
		case "ctrl+c":
			// Priority 1: if input has content, clear it (don't quit)
			if m.editor.Clear(components.ComposerHistorySessionOnly) {
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
				m.addMessage("assistant", "A task is still running. Choose:\n  b — quit and leave it running in the background; return later to view the result\n  c — cancel the task and stay\n  esc — keep watching")
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
			// Enter accepts an eligible completion before the immutable submission
			// preview is read, so history stores the canonical completed command.
			if result := m.editor.HandleKey(msg); result.Action != components.ComposerActionSubmit {
				return m, result.Cmd
			}
			// Ctrl+J is handled above via KeyCtrlJ.
			// Here plain Enter submits
			// Use ExpandValue() to replace paste placeholders with actual content.
			// The display form (with compact [Paste #N · size] / [Image #N · name] tokens)
			// is what the transcript echoes; the expanded form is what runs.
			preview := m.editor.PreviewSubmission()
			display := preview.Display
			input := preview.Expanded
			if input == "" {
				return m, nil
			}
			// A placeholder that survived expansion means its payload can no
			// longer be recovered (an edited token or unmatched current-format
			// token). Refuse locally and KEEP the composer: resetting it here is
			// what previously turned the daemon's "paste it again" into a dead
			// end, because the snippet buffer was already gone.
			if stranded := preview.Unresolved; stranded != "" {
				m.addMessage("notice", "This paste placeholder lost its content and was not sent: "+stranded+"\nThe text is still in your clipboard — delete the placeholder and paste it again.")
				return m, nil
			}
			if m.modelGatewayOffline && !localCommandDuringModelRestart(input) {
				m.setStatusNotice(noticeWarning, "The gateway is restarting. Your draft is preserved; submit it after the model change is healthy.")
				return m, nil
			}
			submission := m.editor.Submit(composerHistoryDisposition(input))
			if submission.Persist {
				m.inputHistoryStore.Append(submission.PersistentText)
			}

			if m.clarifyMode {
				response := m.resolveClarifyResponse(input)
				m.addMessage("user", response)
				if m.clarifyGateway {
					m.clarifyMode = false
					m.clarifyGateway = false
					m.clarifyChoices = nil
					m.clarifyReq = tools.ClarifyRequest{}
					m.runStatus = "working"
					waitCmd := m.startModelWait("Waiting for the model to continue the task")
					return m, tea.Batch(m.answerClarifyViaGateway(response), waitCmd, workingTick())
				}
				m.clarifyBridge.Submit(m.clarifyReq, response)
				m.clarifyMode = false
				m.clarifyGateway = false
				m.clarifyChoices = nil
				m.clarifyReq = tools.ClarifyRequest{}
				m.runStatus = "working"
				m.runTokens = 0
				waitCmd := m.startModelWait("Waiting for the model to respond")
				return m, tea.Batch(waitCmd, workingTick())
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
			m.activePlanJSON = ""
			m.steerCh = make(chan string, 16)
			m.localRequestActive = true
			m.localRequestInput = input
			m.runStatus = "working"
			m.runTokens = 0
			waitCmd := m.startModelWait("Waiting for the model to choose the first step")
			ctx, cancel := context.WithCancel(context.Background())
			m.cancelFn = cancel
			ctx = kernel.WithSteering(ctx, m.steerCh)
			return m, tea.Batch(m.runAgent(ctx, input), waitCmd, workingTick())
		}
	}
	result := m.editor.HandleKey(msg)
	return m, result.Cmd
}

func composerHistoryDisposition(input string) components.ComposerHistoryDisposition {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) > 0 && strings.EqualFold(fields[0], "/clear") {
		return components.ComposerHistoryNone
	}
	return components.ComposerHistoryPersistent
}

func localCommandDuringModelRestart(input string) bool {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return true
	}
	switch strings.ToLower(fields[0]) {
	case "/help", "/model", "/clear", "/exit":
		return true
	default:
		return false
	}
}
