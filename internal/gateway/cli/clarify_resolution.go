package cli

import "selfmind/internal/tools"

type MsgClarifyResolved struct {
	ID    string
	Event uiEventRef
}

// A stopped parked Run no longer has a foreground handle. Resolve its exact
// question independently of run lifecycle, without touching another panel.
func (m *uiModel) resolveClarifyElsewhere(msg MsgClarifyResolved) {
	if !m.acceptEvent(msg.Event) || msg.ID == "" {
		return
	}
	if _, pending := m.otherClarifications[msg.ID]; pending {
		delete(m.otherClarifications, msg.ID)
		m.projectOtherSessionQuestion()
		m.refreshOtherSessionNotice()
	}
	if !m.clarifyGateway || m.clarifyReq.ID != msg.ID {
		return
	}
	m.clarifyMode, m.clarifyGateway = false, false
	m.clarifyChoices = nil
	m.clarifyReq = tools.ClarifyRequest{}
	if !m.daemonRunActive && !m.localRequestActive && m.runStatus == "waiting_user" {
		if m.queuedCount > 0 {
			m.runStatus = "queued"
		} else {
			m.runStatus = "idle"
		}
	}
	m.setStatusNotice(noticeInfo, "The question is no longer pending.")
}
