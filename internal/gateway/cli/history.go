package cli

import "strings"

const maxInputHistory = 200

// recordInputHistory records one submitted input for up/down-arrow recall,
// in memory and (via the store) on disk. input must be the EXPANDED text —
// never the display value: paste placeholders ([[ paste:N ... ]]) die with
// the editor's snippet buffer on Reset, so a recalled placeholder would
// re-submit as literal garbage. Secure (password) input is never recorded.
// Oversized inputs (large pastes) are skipped entirely — recalling megabytes
// into the composer is never what the user meant.
func (m *uiModel) recordInputHistory(input string) {
	if m == nil {
		return
	}
	if m.editor != nil && m.editor.IsSecure() {
		return
	}
	input = strings.TrimSpace(input)
	if input == "" || len(input) > maxInputHistoryEntryBytes {
		return
	}
	if len(m.inputHistory) > 0 && m.inputHistory[len(m.inputHistory)-1] == input {
		m.historyIndex = -1
		m.historyDraft = ""
		return
	}
	m.inputHistoryStore.Append(input)
	m.inputHistory = append(m.inputHistory, input)
	if len(m.inputHistory) > maxInputHistory {
		m.inputHistory = append([]string{}, m.inputHistory[len(m.inputHistory)-maxInputHistory:]...)
	}
	m.historyIndex = -1
	m.historyDraft = ""
}

func (m *uiModel) navigateInputHistory(delta int) bool {
	if m == nil || m.editor == nil || m.editor.IsSecure() || len(m.inputHistory) == 0 {
		return false
	}

	current := m.editor.Value()
	if m.historyIndex == -1 && strings.Contains(current, "\n") {
		return false
	}

	switch {
	case delta < 0:
		if m.historyIndex == -1 {
			m.historyDraft = current
			m.historyIndex = len(m.inputHistory) - 1
		} else if m.historyIndex > 0 {
			m.historyIndex--
		}
		m.editor.SetValue(m.inputHistory[m.historyIndex])
		return true
	case delta > 0:
		if m.historyIndex == -1 {
			return false
		}
		if m.historyIndex < len(m.inputHistory)-1 {
			m.historyIndex++
			m.editor.SetValue(m.inputHistory[m.historyIndex])
			return true
		}
		m.historyIndex = -1
		m.editor.SetValue(m.historyDraft)
		m.historyDraft = ""
		return true
	default:
		return false
	}
}
