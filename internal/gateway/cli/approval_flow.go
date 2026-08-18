package cli

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/tools"
	"selfmind/internal/ui/components"
)

// TUI approval flow: a Codex-style interactive panel in the ACTIVE region
// (docs/tui-terminal-first-hybrid.md §3 lists the approval dialog as active
// content). The controller only wires state here; the selectable panel itself
// is the reusable components.ApprovalPrompt. IM/text approval surfaces are
// untouched — this file is TUI-only presentation over the same gateway
// approval lifecycle (POST /v1/approvals/respond with the grant Scope).

// approvalTypingIdleDelay holds an arriving approval panel back until the person
// has stopped typing for this long. Without it a panel can appear between two
// keystrokes and eat the next one as a decision — "y" is both a common letter
// and "yes, run it". The panel consumes every key once it is up
// (handleApprovalPromptKey), so the guard has to happen BEFORE it arms.
const approvalTypingIdleDelay = 900 * time.Millisecond

// MsgApprovalDelayElapsed fires when the typing-idle window has passed, so a
// held-back approval can be re-considered for display.
type MsgApprovalDelayElapsed struct{}

// noteInputActivity records a keystroke for the typing-idle guard.
func (m *uiModel) noteInputActivity(at time.Time) {
	m.lastInputActivityAt = at
}

// approvalDelayRemaining reports how long an approval must still wait before it
// may steal the keyboard, or 0 when it may arm immediately.
func (m *uiModel) approvalDelayRemaining(now time.Time) time.Duration {
	if m.lastInputActivityAt.IsZero() {
		return 0
	}
	if elapsed := now.Sub(m.lastInputActivityAt); elapsed < approvalTypingIdleDelay {
		return approvalTypingIdleDelay - elapsed
	}
	return 0
}

// holdApprovalRequest parks an approval until typing goes idle and returns the
// tick that will re-check. The request stays durable in the daemon either way —
// this only delays the LOCAL prompt, never the approval itself.
func (m *uiModel) holdApprovalRequest(msg MsgApprovalRequest, wait time.Duration) tea.Cmd {
	m.delayedApprovals = append(m.delayedApprovals, msg)
	return tea.Tick(wait, func(time.Time) tea.Msg { return MsgApprovalDelayElapsed{} })
}

// releaseDelayedApprovals arms the oldest held-back approval once typing has been
// idle long enough; it re-schedules itself while the person is still typing, and
// waits for the current decision when a panel is already up.
func (m *uiModel) releaseDelayedApprovals(now time.Time) tea.Cmd {
	if len(m.delayedApprovals) == 0 {
		return nil
	}
	if remaining := m.approvalDelayRemaining(now); remaining > 0 {
		return tea.Tick(remaining, func(time.Time) tea.Msg { return MsgApprovalDelayElapsed{} })
	}
	next := m.delayedApprovals[0]
	m.delayedApprovals = m.delayedApprovals[1:]
	if m.approvalFlowActive() {
		// A panel is already up: fall back to the normal FIFO queue so the
		// held request is not lost, and keep draining the rest.
		m.approvalQueue = append(m.approvalQueue, next)
	} else {
		m.armApprovalPrompt(next)
	}
	if len(m.delayedApprovals) > 0 {
		return tea.Tick(approvalTypingIdleDelay, func(time.Time) tea.Msg { return MsgApprovalDelayElapsed{} })
	}
	return nil
}

// approvalFlowActive reports whether an approval is awaiting a user decision
// (panel up or more requests queued). The status bar keys off this to show the
// waiting-approval state.
func (m *uiModel) approvalFlowActive() bool {
	return m.approvalPrompt != nil || len(m.approvalQueue) > 0
}

// armApprovalPrompt shows the interactive panel for one approval request and
// records ONE compact transcript line as the durable record. It deliberately
// does not set the legacy "type y to allow" notice — the panel is the prompt.
func (m *uiModel) armApprovalPrompt(msg MsgApprovalRequest) {
	m.pendingApprovalID = msg.ID
	m.pendingApprovalTool = msg.Tool
	m.approvalPrompt = components.NewApprovalPromptDetailed(components.ApprovalDetails{
		Tool:          msg.Tool,
		Target:        msg.Target,
		Reason:        msg.Reason,
		Parked:        strings.EqualFold(strings.TrimSpace(msg.WaiterState), "parked"),
		Environment:   msg.Environment,
		Cwd:           msg.Cwd,
		ChangeSummary: msg.ChangeSummary,
		GrantClass:    msg.GrantClass,
		Containment:   msg.Containment,
		CodePreview:   msg.CodePreview,
		CodeSHA256:    msg.CodeSHA256,
		CodeLines:     msg.CodeLines,
		CodeBytes:     msg.CodeBytes,
		// Only the explicit "triage could not rule" state earns the notice. A
		// deliberate escalation is the funnel working, and saying "unavailable"
		// there would be a lie the person would learn to ignore.
		TriageUnavailable: msg.TriageState == tools.TriageStateUnavailable,
		TriageRationale:   msg.Rationale,
		TriageRisk:        msg.Risk,
		Options:           msg.Options,
	})
	record := "⚠ Approval required: " + msg.Tool
	if reason := strings.TrimSpace(msg.Reason); reason != "" {
		record += " — " + reason
	}
	m.addNotice(noticeWarning, record)
}

// hasApprovalRequest keeps digest hydration and event replay idempotent. A
// request can be active, queued, or delayed behind typing; all three are one UI
// ledger keyed by the daemon-issued approval id.
func (m *uiModel) hasApprovalRequest(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if m.pendingApprovalID == id {
		return true
	}
	for _, request := range m.approvalQueue {
		if request.ID == id {
			return true
		}
	}
	for _, request := range m.delayedApprovals {
		if request.ID == id {
			return true
		}
	}
	return false
}

func (m *uiModel) markApprovalParked(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if m.pendingApprovalID == id && m.approvalPrompt != nil {
		m.approvalPrompt.SetParked(true)
	}
	for i := range m.approvalQueue {
		if m.approvalQueue[i].ID == id {
			m.approvalQueue[i].WaiterState = "parked"
		}
	}
	for i := range m.delayedApprovals {
		if m.delayedApprovals[i].ID == id {
			m.delayedApprovals[i].WaiterState = "parked"
		}
	}
	m.setStatusNotice(noticeWarning, "Approval is still waiting; the task is parked until you answer.")
}

// armNextQueuedApproval re-arms the panel with the next pending request (FIFO)
// after the current one is resolved, so back-to-back approvals never get lost.
func (m *uiModel) armNextQueuedApproval() {
	if len(m.approvalQueue) == 0 {
		return
	}
	next := m.approvalQueue[0]
	m.approvalQueue = m.approvalQueue[1:]
	m.armApprovalPrompt(next)
}

// clearApprovalFlow drops all pending approval UI state after explicit
// resolution or local teardown.
func (m *uiModel) clearApprovalFlow() {
	m.approvalPrompt = nil
	m.approvalQueue = nil
	m.delayedApprovals = nil
	m.pendingApprovalID = ""
	m.pendingApprovalTool = ""
}

// settleApprovalFlowAtRunEnd removes requests tied to dead live waiters while
// preserving parked requests. A parked approval intentionally outlives its
// original run; dropping that panel here would make the database answerable but
// the current terminal unable to answer until it reconnects and fetches a new
// digest.
func (m *uiModel) settleApprovalFlowAtRunEnd() {
	parkedQueue := make([]MsgApprovalRequest, 0, len(m.approvalQueue)+len(m.delayedApprovals))
	for _, request := range m.approvalQueue {
		if request.WaiterState == "parked" {
			parkedQueue = append(parkedQueue, request)
		}
	}
	for _, request := range m.delayedApprovals {
		if request.WaiterState == "parked" {
			parkedQueue = append(parkedQueue, request)
		}
	}
	m.delayedApprovals = nil
	if m.approvalPrompt != nil && m.approvalPrompt.IsParked() {
		m.approvalQueue = parkedQueue
		return
	}
	m.approvalPrompt = nil
	m.pendingApprovalID = ""
	m.pendingApprovalTool = ""
	m.approvalQueue = parkedQueue
	m.armNextQueuedApproval()
}

// handleApprovalPromptKey routes a key press into the active panel. Like Codex,
// Esc and Ctrl+C are explicit cancellation decisions: neither may dismiss an
// unanswered approval silently. Every other key is consumed by the panel so a
// stray keystroke cannot leak into the composer.
func (m *uiModel) handleApprovalPromptKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	key := msg.String()
	if key == "esc" || key == "ctrl+c" {
		return m.cancelCurrentApproval(), true
	}
	choice := m.approvalPrompt.HandleKey(key)
	if choice == nil {
		return nil, true
	}
	return m.resolveApprovalChoice(*choice), true
}

// resolveApprovalChoice applies the selected panel option immediately. The
// daemon-issued label says exactly what is granted; the client does not invent
// or broaden authority.
func (m *uiModel) resolveApprovalChoice(opt components.ApprovalOption) tea.Cmd {
	if opt.Decision == "rejected" {
		return m.rejectCurrentApproval(false)
	}
	tool := m.pendingApprovalTool
	if err := m.sendApprovalDecision("approved", opt.Scope, opt.GrantKey); err != nil {
		m.addErrorMessage(fmt.Sprintf("Could not send approval: %v", err))
		id := m.setStatusNotice(noticeWarning, "Approval was not accepted; try again.")
		return clearStatusNoticeAfter(id, 3*time.Second)
	}
	m.approvalPrompt = nil
	m.addNotice(noticeSuccess, "✓ Approved "+tool+approvalDecisionNote(opt))
	noticeID := m.setStatusNotice(noticeSuccess, "Approved "+tool+" — resuming.")
	m.resumeAfterApproval()
	m.armNextQueuedApproval()
	return tea.Batch(m.spinner.Tick, workingTick(), clearStatusNoticeAfter(noticeID, 1500*time.Millisecond))
}

func (m *uiModel) cancelCurrentApproval() tea.Cmd {
	return m.rejectCurrentApproval(true)
}

// rejectCurrentApproval sends a durable rejection before changing local UI
// state. Delivery failure therefore leaves the same panel and decision intact
// for retry. A cancelled panel and an explicit "No" share the daemon's reject
// contract, but keep distinct transcript wording.
func (m *uiModel) rejectCurrentApproval(cancelled bool) tea.Cmd {
	tool := m.pendingApprovalTool
	if err := m.sendApprovalDecision("rejected", "", ""); err != nil {
		m.addErrorMessage(fmt.Sprintf("Could not send rejection: %v", err))
		id := m.setStatusNotice(noticeWarning, "Rejection was not accepted; try again.")
		return clearStatusNoticeAfter(id, 3*time.Second)
	}
	m.approvalPrompt = nil
	record, status := "✗ Denied "+tool, "Denied."
	if cancelled {
		record = "✗ Cancelled approval for " + tool
		status = "Approval cancelled."
	}
	m.addNotice(noticeError, record)
	noticeID := m.setStatusNotice(noticeError, status)
	m.resumeAfterApproval()
	m.armNextQueuedApproval()
	return tea.Batch(m.spinner.Tick, workingTick(), clearStatusNoticeAfter(noticeID, 1500*time.Millisecond))
}

// sendApprovalDecision answers the pending approval through the SAME path the
// TUI has always used (the installed responder → POST /v1/approvals/respond),
// now carrying the grant scope. Failures surface honestly in the transcript —
// a decision must never look delivered when it was not.
func (m *uiModel) sendApprovalDecision(decision, scope, grantKey string) error {
	id := m.pendingApprovalID
	if id == "" {
		return fmt.Errorf("no pending approval id is available")
	}
	if m.approvalResponder == nil {
		return fmt.Errorf("no approval responder is available in this session")
	}
	if err := m.approvalResponder(id, decision, scope, grantKey); err != nil {
		return err
	}
	m.rememberLocalApprovalResolution(id, decision)
	m.pendingApprovalID = ""
	m.pendingApprovalTool = ""
	return nil
}

const localApprovalResolutionLimit = 64

func (m *uiModel) rememberLocalApprovalResolution(id, status string) {
	id = strings.TrimSpace(id)
	status = strings.TrimSpace(status)
	if id == "" || status == "" {
		return
	}
	if m.localApprovalResolutions == nil {
		m.localApprovalResolutions = make(map[string]string)
	}
	if _, exists := m.localApprovalResolutions[id]; !exists {
		m.localApprovalOrder = append(m.localApprovalOrder, id)
	}
	m.localApprovalResolutions[id] = status
	for len(m.localApprovalOrder) > localApprovalResolutionLimit {
		oldest := m.localApprovalOrder[0]
		m.localApprovalOrder = m.localApprovalOrder[1:]
		delete(m.localApprovalResolutions, oldest)
	}
}

func (m *uiModel) consumeLocalApprovalResolution(id, status string) bool {
	id = strings.TrimSpace(id)
	want, ok := m.localApprovalResolutions[id]
	if !ok || want != strings.TrimSpace(status) {
		return false
	}
	delete(m.localApprovalResolutions, id)
	for i, candidate := range m.localApprovalOrder {
		if candidate == id {
			m.localApprovalOrder = append(m.localApprovalOrder[:i], m.localApprovalOrder[i+1:]...)
			break
		}
	}
	return true
}

// resolveApprovalElsewhere reconciles a person-stream resolution against the
// active panel and both queues. It returns a command only for a genuinely
// external answer; the stream echo of this client's own answer is silent.
func (m *uiModel) resolveApprovalElsewhere(msg MsgApprovalResolved) tea.Cmd {
	id := strings.TrimSpace(msg.ID)
	status := strings.TrimSpace(msg.Status)
	if id == "" || (status != "approved" && status != "rejected" && status != "expired" && status != "archived") {
		return nil
	}
	localEcho := status != "expired" && status != "archived" && m.consumeLocalApprovalResolution(id, status)
	tool := ""
	active := m.pendingApprovalID == id
	known := active
	if active {
		tool = m.pendingApprovalTool
		m.approvalPrompt = nil
		m.pendingApprovalID = ""
		m.pendingApprovalTool = ""
	}
	var removed bool
	m.approvalQueue, tool, removed = removeApprovalRequest(m.approvalQueue, id, tool)
	known = known || removed
	m.delayedApprovals, tool, removed = removeApprovalRequest(m.delayedApprovals, id, tool)
	known = known || removed
	if localEcho {
		return nil
	}
	if !known {
		return nil
	}
	label := strings.TrimSpace(tool)
	if label == "" {
		label = "request"
	}
	kind := noticeSuccess
	record := "✓ Approved " + label + " elsewhere"
	statusText := "Approval resolved elsewhere."
	switch status {
	case "rejected":
		kind = noticeError
		record = "✗ Denied " + label + " elsewhere"
		statusText = "Approval was denied elsewhere."
	case "expired":
		kind = noticeWarning
		record = "⚠ Approval expired: " + label
		statusText = "Approval is no longer answerable."
	case "archived":
		kind = noticeWarning
		record = "⚠ Approval archived after 7 days: " + label
		statusText = "Approval was archived; resume the task to re-evaluate it."
	}
	m.addNotice(kind, record)
	noticeID := m.setStatusNotice(kind, statusText)
	ttl := 1500 * time.Millisecond
	if status == "expired" || status == "archived" {
		ttl = 3 * time.Second
	}
	cmds := []tea.Cmd{clearStatusNoticeAfter(noticeID, ttl)}
	if active {
		m.armNextQueuedApproval()
		if status != "expired" && status != "archived" {
			m.resumeAfterApproval()
			cmds = append(cmds, m.spinner.Tick, workingTick())
		}
	}
	return tea.Batch(cmds...)
}

func removeApprovalRequest(queue []MsgApprovalRequest, id, tool string) ([]MsgApprovalRequest, string, bool) {
	if len(queue) == 0 {
		return queue, tool, false
	}
	out := queue[:0]
	removed := false
	for _, request := range queue {
		if request.ID == id {
			removed = true
			if strings.TrimSpace(tool) == "" {
				tool = request.Tool
			}
			continue
		}
		out = append(out, request)
	}
	return out, tool, removed
}

// resumeAfterApproval puts the UI back into the working state: the blocked run
// resumes as soon as the decision lands on the daemon.
func (m *uiModel) resumeAfterApproval() {
	m.thinking = true
	m.runStatus = "working"
	m.thinkingStart = time.Now()
	// The pre-approval activity label ("Preparing to run <tool>.") is stale
	// noise now; the next agent/tool event repopulates it.
	m.activityText = ""
}

// approvalDecisionNote is the transcript suffix for an approve decision. A rule
// decision names the RULE (that is what the person actually chose); a class
// decision falls back to the scope wording.
func approvalDecisionNote(opt components.ApprovalOption) string {
	if strings.TrimSpace(opt.RuleLabel) != "" {
		return " (allowed for this run: " + opt.RuleLabel + ")"
	}
	return approvalScopeNote(opt.Scope)
}

// approvalScopeNote is the human-readable suffix for an approve decision's
// grant scope in the transcript record.
func approvalScopeNote(scope string) string {
	switch scope {
	case "run":
		return " (allowed for this run)"
	case "task":
		return " (allowed for this task)"
	case "person":
		return " (allowed across tasks for 8h)"
	default:
		return " (this time)"
	}
}

// approvalOptionsFromPayload maps the daemon's server-issued decision list off an
// approval.requested event into panel options (batch B1). The daemon owns what a
// person may choose; the TUI only renders it and hands the chosen key back. A
// malformed or absent list yields nil, which makes the panel fall back to its
// built-in options rather than showing nothing.
func approvalOptionsFromPayload(payload map[string]interface{}) []components.ApprovalOption {
	raw, ok := payload["decisions"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil
	}
	options := make([]components.ApprovalOption, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		text := func(key string) string {
			value, _ := item[key].(string)
			return strings.TrimSpace(value)
		}
		label, decision := text("label"), text("decision")
		if label == "" || decision == "" {
			continue
		}
		options = append(options, components.ApprovalOption{
			Label:     label,
			Key:       text("key"),
			Decision:  decision,
			Scope:     text("scope"),
			GrantKey:  text("grant_key"),
			RuleLabel: text("rule_label"),
		})
	}
	if len(options) == 0 {
		return nil
	}
	return options
}
