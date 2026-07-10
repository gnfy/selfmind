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

func (m *uiModel) appendToolOutput(toolName, content string) {
	content = textutil.CleanUTF8(content)
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != "tool" {
		m.addMessage("tool", "")
	}
	last := &m.messages[len(m.messages)-1]
	if toolName != "" {
		last.ToolName = toolName
	}
	last.IsRunning = true
	if strings.TrimSpace(last.Content) == "" {
		last.Content = content
		return
	}
	last.Content += "\n" + content
}

func (m *uiModel) findToolMessageIndex(toolCallID, toolName string) int {
	if toolCallID != "" {
		for i := len(m.messages) - 1; i >= 0; i-- {
			msg := m.messages[i]
			if msg.Role == "tool" && msg.ToolCallID == toolCallID {
				return i
			}
		}
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Role != "tool" || !msg.IsRunning {
			continue
		}
		if toolName == "" || msg.ToolName == "" || msg.ToolName == toolName {
			return i
		}
	}
	if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == "tool" {
		return len(m.messages) - 1
	}
	return -1
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
