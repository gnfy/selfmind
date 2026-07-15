package cli

import (
	"strings"
	"time"

	"selfmind/internal/platform/textutil"
)

func (m *uiModel) addMessage(role, content string) {
	content = textutil.CleanUTF8(content)
	m.messages = append(m.messages, ChatMessage{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	})
	// In hybrid mode a non-tool message is final on arrival, so commit it to
	// scrollback now. Tool messages start as "running" and are committed later
	// by MsgToolDone.
	if role != "tool" {
		m.commit(&m.messages[len(m.messages)-1])
	}
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

const maxActiveToolOutputLines = 3

func (m *uiModel) appendToolOutput(toolCallID, toolName, content string) bool {
	content = textutil.CleanUTF8(content)
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return false
	}
	idx := m.findActiveToolMessageIndex(toolCallID, toolName)
	if idx < 0 {
		// Output can arrive after a run has finalized or after an SSE reconnect.
		// Never create an anonymous tool row for an event that cannot be tied to
		// an active call; it would stay in the redraw region forever.
		return false
	}
	last := &m.messages[idx]
	if toolName != "" {
		last.ToolName = toolName
	}
	if toolCallID != "" {
		last.ToolCallID = toolCallID
	}
	combined := content
	if existing := strings.TrimSpace(last.Content); existing != "" {
		combined = existing + "\n" + content
	}
	lines := strings.Split(combined, "\n")
	if len(lines) > maxActiveToolOutputLines {
		lines = lines[len(lines)-maxActiveToolOutputLines:]
	}
	last.Content = strings.Join(lines, "\n")
	return true
}

func (m *uiModel) findActiveToolMessageIndex(toolCallID, toolName string) int {
	if toolCallID != "" {
		for i := len(m.messages) - 1; i >= 0; i-- {
			msg := m.messages[i]
			if msg.Role == "tool" && !msg.Committed && msg.IsRunning && msg.ToolCallID == toolCallID {
				return i
			}
		}
		return -1
	}
	match := -1
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Role != "tool" || msg.Committed || !msg.IsRunning {
			continue
		}
		if toolName == "" || msg.ToolName == "" || msg.ToolName == toolName {
			if match >= 0 {
				// A legacy event without a call id is safe only when it identifies one
				// active call. Guessing between parallel calls corrupts both rows.
				return -1
			}
			match = i
		}
	}
	return match
}

func (m *uiModel) toolMessageExists(toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == "tool" && m.messages[i].ToolCallID == toolCallID {
			return true
		}
	}
	return false
}

func (m *uiModel) discardOpenToolMessages() {
	kept := m.messages[:0]
	for _, msg := range m.messages {
		if msg.Role == "tool" && !msg.Committed {
			continue
		}
		kept = append(kept, msg)
	}
	m.messages = kept
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "done", "error", "cancelled":
		return true
	default:
		return false
	}
}

func (m *uiModel) appendAssistantResponse(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if len(m.messages) > 0 {
		last := &m.messages[len(m.messages)-1]
		// Never merge into a committed cell — in hybrid mode it already lives in
		// immutable scrollback and cannot be rewritten.
		if last.Role == "assistant" && !last.IsError && !last.Committed {
			existing := strings.TrimSpace(last.Content)
			switch {
			case existing == "":
				last.Content = content
				return
			case existing == content:
				return
			case strings.HasSuffix(existing, content):
				return
			}
		}
	}
	m.addMessage("assistant", content)
}

func (m *uiModel) commitLiveStream(content string) bool {
	content = textutil.CleanUTF8(content)
	if strings.TrimSpace(content) == "" {
		return false
	}
	m.liveStreamContent += content
	return true
}

func (m *uiModel) flushLiveStreamPending() bool {
	return m.commitLiveStream(m.streamController.Flush())
}

func (m *uiModel) clearLiveStream() {
	m.streamController.Reset()
	m.liveStreamContent = ""
	m.streamFlushPending = false
}

func (m *uiModel) finalizeLiveStream(finalContent string) bool {
	m.flushLiveStreamPending()
	live := strings.TrimSpace(m.liveStreamContent)
	finalContent = strings.TrimSpace(textutil.CleanUTF8(finalContent))
	content := finalContent
	if content == "" {
		content = live
	}
	m.clearLiveStream()
	if content == "" {
		return false
	}
	m.appendAssistantResponse(content)
	return true
}
