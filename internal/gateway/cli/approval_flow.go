package cli

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	m.approvalPrompt = components.NewApprovalPrompt(msg.Tool, msg.Target, msg.Reason)
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
	m.sendApprovalDecision("approved", opt.Scope)
	m.addMessage("notice", "✓ Approved "+tool+approvalScopeNote(opt.Scope))
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
	m.sendApprovalDecision("rejected", "")
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
func (m *uiModel) sendApprovalDecision(decision, scope string) {
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
	if err := m.approvalResponder(id, decision, scope); err != nil {
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
