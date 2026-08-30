package cli

import (
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/textutil"
)

func (m *uiModel) addMessage(role, content string) {
	content = textutil.CleanUTF8(content)
	m.messages = append(m.messages, ChatMessage{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	})
	// Mutable tool projections live exclusively in processSurface. Everything
	// admitted to transcript history is final and can be committed immediately.
	m.commit(&m.messages[len(m.messages)-1])
	m.trimHistoryWindow()
}

func (m *uiModel) addNotice(kind noticeKind, content string) {
	content = textutil.CleanUTF8(content)
	m.messages = append(m.messages, ChatMessage{
		Role:       "notice",
		Content:    content,
		Timestamp:  time.Now(),
		NoticeKind: kind,
	})
	m.commit(&m.messages[len(m.messages)-1])
	m.trimHistoryWindow()
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
}

func (m *uiModel) applyProcessEffects(effects processEffects) {
	for _, message := range effects.commits {
		message.Content = textutil.CleanUTF8(message.Content)
		if strings.TrimSpace(message.Content) == "" {
			continue
		}
		if message.Role == "assistant" && len(m.messages) > 0 {
			last := &m.messages[len(m.messages)-1]
			samePhase := last.AssistantPhase == message.AssistantPhase || last.AssistantPhase == llm.AssistantPhaseUnspecified
			if last.Role == "assistant" && !last.IsError && samePhase && strings.TrimSpace(last.Content) == strings.TrimSpace(message.Content) {
				if last.AssistantPhase == llm.AssistantPhaseUnspecified {
					last.AssistantPhase = message.AssistantPhase
				}
				if !last.Committed {
					m.commit(last)
				}
				continue
			}
		}
		if message.Timestamp.IsZero() {
			message.Timestamp = time.Now()
		}
		m.messages = append(m.messages, message)
		m.commit(&m.messages[len(m.messages)-1])
	}
	m.trimHistoryWindow()
}

const maxActiveToolOutputLines = 3

func (m *uiModel) discardOpenToolMessages() {
	m.applyProcessEffects(m.processState().Update(processEvent{kind: processToolsDiscarded}))
}

// finalizeOpenToolMessages is the single terminal cleanup path for mutable tool
// cells. A run may end without every tool.completed event reaching this client
// (cancellation, reconnect gap, producer defect). Preserve that fact in
// immutable history instead of silently deleting the evidence or leaving a
// permanent Running row in the active redraw region.
func (m *uiModel) finalizeOpenToolMessages(reason string) int {
	effects := m.processState().Update(processEvent{kind: processToolsInterrupted, reason: reason})
	m.applyProcessEffects(effects)
	return len(effects.commits)
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "done", "error", "cancelled":
		return true
	default:
		return false
	}
}

func (m *uiModel) appendAssistantResponse(content string, phases ...llm.AssistantPhase) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	phase := llm.AssistantPhaseFinalAnswer
	if len(phases) > 0 && phases[0] != llm.AssistantPhaseUnspecified {
		phase = phases[0]
	}
	if len(m.messages) > 0 {
		last := &m.messages[len(m.messages)-1]
		if last.Role == "assistant" && !last.IsError && last.AssistantPhase == phase && strings.TrimSpace(last.Content) == content {
			return
		}
		// Never merge into a committed cell — in hybrid mode it already lives in
		// immutable scrollback and cannot be rewritten.
		if last.Role == "assistant" && !last.IsError && !last.Committed {
			existing := strings.TrimSpace(last.Content)
			switch {
			case existing == "":
				last.Content = content
				last.AssistantPhase = phase
				return
			case existing == content:
				return
			case strings.HasSuffix(existing, content):
				return
			}
		}
	}
	m.messages = append(m.messages, ChatMessage{
		Role:           "assistant",
		Content:        textutil.CleanUTF8(content),
		AssistantPhase: phase,
		Timestamp:      time.Now(),
	})
	m.commit(&m.messages[len(m.messages)-1])
	m.trimHistoryWindow()
}

func (m *uiModel) commitLiveStream(content string) bool {
	content = textutil.CleanUTF8(content)
	if strings.TrimSpace(content) == "" {
		return false
	}
	m.applyProcessEffects(m.processState().Update(processEvent{kind: processStreamDelta, content: content}))
	return true
}

func (m *uiModel) clearLiveStream() {
	m.processState().ClearStream()
}

func (m *uiModel) finalizeLiveStream(finalContent string, phases ...llm.AssistantPhase) bool {
	finalContent = strings.TrimSpace(textutil.CleanUTF8(finalContent))
	phase := llm.AssistantPhaseFinalAnswer
	if len(phases) > 0 && phases[0] != llm.AssistantPhaseUnspecified {
		phase = phases[0]
	}
	effects := m.processState().Update(processEvent{
		kind: processFinished, content: finalContent, phase: phase,
	})
	m.applyProcessEffects(effects)
	return len(effects.commits) > 0
}
