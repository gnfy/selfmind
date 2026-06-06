package cli

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *uiModel) copySelection() tea.Cmd {
	start := m.selectionStart
	end := m.selectionEnd
	if start > end {
		start, end = end, start
	}
	viewportTop := 0
	fullLines := m.renderAllMessagesLines()
	scrollOffset := m.viewport.YOffset
	lineStart := scrollOffset + (start - viewportTop)
	lineEnd := scrollOffset + (end - viewportTop)
	if lineStart < 0 {
		lineStart = 0
	}
	if lineEnd >= len(fullLines) {
		lineEnd = len(fullLines) - 1
	}
	if lineStart > lineEnd {
		return nil
	}
	var cleanLines []string
	for _, line := range fullLines[lineStart : lineEnd+1] {
		clean := strings.TrimRight(stripANSI(line), " ")
		cleanLines = append(cleanLines, clean)
	}
	selectedText := strings.Join(cleanLines, "\n")
	if selectedText == "" {
		return nil
	}

	return func() tea.Msg {
		b64 := base64.StdEncoding.EncodeToString([]byte(selectedText))
		fmt.Printf("\x1b]52;c;%s\a", b64)

		m.statusMsg = "Selected text copied to clipboard"
		go func() {
			time.Sleep(2 * time.Second)
			if m.program != nil {
				m.program.Send(MsgClearStatus{})
			}
		}()
		return nil
	}
}

func (m *uiModel) renderAllMessagesLines() []string {
	content := m.renderAllMessages()
	return strings.Split(content, "\n")
}
