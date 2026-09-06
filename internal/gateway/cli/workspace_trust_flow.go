package cli

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/ui/components"
)

// handleWorkspaceTrustPromptKey routes a key to the armed trust question and
// reports whether it was consumed. Like the approval panel, it captures every
// key while armed: a question that stray keystrokes can slip past is the notice
// this replaced.
func (m *uiModel) handleWorkspaceTrustPromptKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	key := msg.String()
	if key == "ctrl+c" {
		return nil, false
	}
	answer := m.workspaceTrustPrompt.HandleKey(key)
	if answer == nil {
		return nil, true
	}
	return m.resolveWorkspaceTrustAnswer(*answer), true
}

// resolveWorkspaceTrustAnswer applies one answer. "Not now" is settled locally:
// deferring is not a decision the daemon should record, so the question returns
// in the next session. The other two answers are the gateway's own explicit
// controls, sent unchanged — the client never sets a trust level itself.
func (m *uiModel) resolveWorkspaceTrustAnswer(answer components.WorkspaceTrustOption) tea.Cmd {
	m.workspaceTrustPrompt = nil
	if answer.Command == "" {
		name := "This workspace"
		if m.sessionWorkspace != nil && strings.TrimSpace(m.sessionWorkspace.Name) != "" {
			name = m.sessionWorkspace.Name
		}
		m.addNotice(noticeInfo, name+" stays untrusted for now — SelfMind will ask again next time.")
		return nil
	}
	processor := m.messageProcessor
	if processor == nil {
		m.addErrorMessage("Gateway not initialized; workspace trust was not changed.")
		return nil
	}
	request := m.controlMessageRequest(answer.Command)
	return func() tea.Msg {
		response, _ := processor(context.Background(), request)
		if response.Error != "" {
			return MsgWorkspaceTrustAnswered{Err: fmt.Errorf("%s", response.Error)}
		}
		return MsgWorkspaceTrustAnswered{Reply: response.Content, Workspace: response.Workspace}
	}
}

// workspaceTrustConfirmation states the resulting capability in the terms the
// question used. The daemon's own reply is a CLI line carrying the workspace
// UUID; relaying it into the TUI answered a plain question with an identifier,
// directly under a startup card that still said [untrusted].
func workspaceTrustConfirmation(ws *api.DigestWorkspace) string {
	if ws == nil {
		return ""
	}
	name := strings.TrimSpace(ws.Name)
	if name == "" {
		name = "this workspace"
	}
	if ws.Trusted {
		return "✓ Trusted " + name + " — its Skills and remembered approvals are on."
	}
	return "Left " + name + " untrusted — SelfMind will not ask about it again."
}

// MsgWorkspaceTrustAnswered reports the daemon's response to a trust answer.
type MsgWorkspaceTrustAnswered struct {
	Reply     string
	Workspace *api.DigestWorkspace
	Err       error
}
