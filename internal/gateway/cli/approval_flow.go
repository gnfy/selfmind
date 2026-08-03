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

// approvalDenyHint is the composer hint shown after choosing "No": the user may
// type follow-up guidance for the agent, or press Enter to just deny.
const approvalDenyHint = "Tell the agent what to do instead — or press Enter to just deny."

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
// (panel up, deny follow-up pending, or more requests queued). The status bar
// keys off this to show the waiting-approval state.
func (m *uiModel) approvalFlowActive() bool {
	return m.approvalPrompt != nil || m.approvalDenyFollowup || len(m.approvalQueue) > 0
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
		Environment:   msg.Environment,
		Cwd:           msg.Cwd,
		ChangeSummary: msg.ChangeSummary,
		GrantClass:    msg.GrantClass,
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
	m.addMessage("notice", record)
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

// clearApprovalFlow drops all pending approval UI state. Called when the run
// ends: the daemon expires unanswered approval rows when their waiter is gone,
// so a panel left up would answer into the void.
func (m *uiModel) clearApprovalFlow() {
	m.approvalPrompt = nil
	m.approvalQueue = nil
	m.delayedApprovals = nil
	m.approvalDenyFollowup = false
	m.pendingApprovalID = ""
	m.pendingApprovalTool = ""
}

// handleApprovalPromptKey routes a key press into the active panel. handled is
// false only for ctrl+c, which keeps its normal semantics (clear input /
// cancel / exit) even while an approval is pending; every other key is
// consumed by the panel so a stray keystroke can never half-answer or leak
// into the composer.
func (m *uiModel) handleApprovalPromptKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	key := msg.String()
	if key == "ctrl+c" {
		return nil, false
	}
	choice := m.approvalPrompt.HandleKey(key)
	if choice == nil {
		return nil, true
	}
	return m.resolveApprovalChoice(*choice), true
}

// resolveApprovalChoice applies the selected panel option. Approvals are sent
// immediately with their grant scope; "No" defers the rejection into the deny
// follow-up so the user can attach guidance in the same gesture.
func (m *uiModel) resolveApprovalChoice(opt components.ApprovalOption) tea.Cmd {
	m.approvalPrompt = nil
	if opt.Decision == "rejected" {
		m.approvalDenyFollowup = true
		m.editor.Reset()
		return nil
	}
	tool := m.pendingApprovalTool
	m.sendApprovalDecision("approved", opt.Scope, opt.GrantKey)
	m.addMessage("notice", "✓ Approved "+tool+approvalDecisionNote(opt))
	m.statusMsg = "Approved " + tool + " — resuming."
	m.resumeAfterApproval()
	m.armNextQueuedApproval()
	return tea.Batch(m.spinner.Tick, workingTick())
}

// finishApprovalDeny resolves the deny follow-up on Enter: always send the
// rejection first (the agent's do-not-retry contract), then forward any typed
// text as mid-turn guidance so the agent knows what to do instead.
func (m *uiModel) finishApprovalDeny(input string) tea.Cmd {
	m.approvalDenyFollowup = false
	tool := m.pendingApprovalTool
	guidance := strings.TrimSpace(input)
	m.editor.Reset()
	m.sendApprovalDecision("rejected", "", "")
	m.addMessage("notice", "✗ Denied "+tool)
	var cmds []tea.Cmd
	if guidance != "" {
		cmds = append(cmds, m.injectMidRunGuidance(guidance))
	} else {
		m.statusMsg = "Denied."
	}
	m.resumeAfterApproval()
	m.armNextQueuedApproval()
	cmds = append(cmds, m.spinner.Tick, workingTick())
	return tea.Batch(cmds...)
}

// sendApprovalDecision answers the pending approval through the SAME path the
// TUI has always used (the installed responder → POST /v1/approvals/respond),
// now carrying the grant scope. Failures surface honestly in the transcript —
// a decision must never look delivered when it was not.
func (m *uiModel) sendApprovalDecision(decision, scope, grantKey string) {
	id := m.pendingApprovalID
	m.pendingApprovalID = ""
	m.pendingApprovalTool = ""
	if id == "" {
		return
	}
	if m.approvalResponder == nil {
		m.addErrorMessage("Could not send approval: no approval responder is available in this session.")
		return
	}
	if err := m.approvalResponder(id, decision, scope, grantKey); err != nil {
		m.addErrorMessage(fmt.Sprintf("Could not send approval: %v", err))
	}
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
	if strings.TrimSpace(opt.GrantKey) != "" && strings.TrimSpace(opt.RuleLabel) != "" {
		return " (won't ask again for " + opt.RuleLabel + ")"
	}
	return approvalScopeNote(opt.Scope)
}

// approvalScopeNote is the human-readable suffix for an approve decision's
// grant scope in the transcript record.
func approvalScopeNote(scope string) string {
	switch scope {
	case "task":
		return " (allowed for this task)"
	case "person":
		return " (always allowed)"
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
			RuleLabel: ruleLabelFromOptionLabel(label),
		})
	}
	if len(options) == 0 {
		return nil
	}
	return options
}

// ruleLabelFromOptionLabel recovers the rule wording out of a server label
// ("Yes, and don't ask again for commands that start with `git status`") for the
// transcript record, so the record states what was remembered.
func ruleLabelFromOptionLabel(label string) string {
	const marker = "don't ask again for "
	if idx := strings.Index(label, marker); idx >= 0 {
		return strings.TrimSpace(label[idx+len(marker):])
	}
	return ""
}
