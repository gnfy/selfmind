package cli

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Mouse selection and edge-scroll handling for the transcript viewport.
// Extracted from controller.go to keep that file focused on the core model and
// Bubble Tea message routing (see AGENTS.md: controller.go must not keep
// growing). Behavior is unchanged.

func (m *uiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.mouseInTranscript(msg) {
			m.scrollTranscriptLines(-3)
		}
		return m, nil
	case tea.MouseButtonWheelDown:
		if m.mouseInTranscript(msg) {
			m.scrollTranscriptLines(3)
		}
		return m, nil
	}

	switch msg.Action {
	case tea.MouseActionPress:
		switch {
		case msg.Button == tea.MouseButtonLeft && m.mouseInTranscript(msg):
			m.mouseDragActive = true
			m.mouseAutoScrollDir = 0
			m.mouseSelection = false
			m.mouseSelectAnchor = m.transcriptLineAtMouseY(msg.Y)
			m.mouseSelectFocus = m.mouseSelectAnchor
		case msg.Button == tea.MouseButtonRight && m.mouseSelection:
			return m, m.copyMouseSelection()
		}
		return m, nil
	case tea.MouseActionMotion:
		if !m.mouseDragActive {
			return m, nil
		}
		m.updateMouseSelection(msg.Y)
		m.mouseAutoScrollDir = m.mouseEdgeScrollDirection(msg.Y)
		if m.mouseAutoScrollDir != 0 {
			m.scrollTranscriptLines(m.mouseAutoScrollDir)
			m.updateMouseSelectionAfterAutoScroll()
			return m, m.ensureMouseAutoScroll()
		}
		return m, nil
	case tea.MouseActionRelease:
		m.mouseDragActive = false
		m.mouseAutoScrollDir = 0
		if m.mouseSelectAnchor == m.mouseSelectFocus {
			m.mouseSelection = false
		}
		return m, nil
	default:
		return m, nil
	}
}

func (m *uiModel) transcriptLineAtMouseY(y int) int {
	height := m.transcriptVisibleHeight()
	if y < 0 {
		y = 0
	}
	if y >= height {
		y = height - 1
	}
	return m.viewport.YOffset + y
}

func (m *uiModel) updateMouseSelection(y int) {
	focus := m.transcriptLineAtMouseY(y)
	m.mouseSelectFocus = focus
	if focus != m.mouseSelectAnchor {
		m.mouseSelection = true
	}
}

func (m *uiModel) updateMouseSelectionAfterAutoScroll() {
	if !m.mouseDragActive {
		return
	}
	if m.mouseAutoScrollDir < 0 {
		m.mouseSelectFocus = m.viewport.YOffset
	} else if m.mouseAutoScrollDir > 0 {
		m.mouseSelectFocus = m.viewport.YOffset + m.transcriptVisibleHeight() - 1
	}
	if m.mouseSelectFocus != m.mouseSelectAnchor {
		m.mouseSelection = true
	}
}

func (m *uiModel) mouseEdgeScrollDirection(y int) int {
	height := m.transcriptVisibleHeight()
	switch {
	case y <= 0:
		return -4
	case y <= 2:
		return -2
	case y >= height-1:
		return 4
	case y >= height-3:
		return 2
	default:
		return 0
	}
}

func (m *uiModel) ensureMouseAutoScroll() tea.Cmd {
	if m.mouseScrollTicking || !m.mouseDragActive || m.mouseAutoScrollDir == 0 {
		return nil
	}
	m.mouseScrollTicking = true
	return mouseAutoScrollTick()
}

func (m *uiModel) copyMouseSelection() tea.Cmd {
	text := m.selectedTranscriptText()
	if strings.TrimSpace(text) == "" {
		m.mouseSelection = false
		m.statusMsg = ""
		return nil
	}
	if err := copyToClipboard(text); err != nil {
		m.statusMsg = fmt.Sprintf("Copy failed: %v", err)
	} else {
		m.statusMsg = "Copied selection to clipboard."
		m.mouseSelection = false
	}
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return MsgClearStatus{}
	})
}

func (m *uiModel) selectedTranscriptText() string {
	if !m.mouseSelection {
		return ""
	}
	start, end := m.mouseSelectionRange()
	selectionWasActive := m.mouseSelection
	m.mouseSelection = false
	content := m.renderAllMessages()
	m.mouseSelection = selectionWasActive

	lines := strings.Split(stripANSI(content), "\n")
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	if start > end || start >= len(lines) {
		return ""
	}
	return strings.TrimRight(strings.Join(lines[start:end+1], "\n"), "\n")
}

func (m *uiModel) mouseSelectionRange() (int, int) {
	start, end := m.mouseSelectAnchor, m.mouseSelectFocus
	if start > end {
		start, end = end, start
	}
	return start, end
}
