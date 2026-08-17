package cli

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// noticeKind is the shared semantic vocabulary for transient notification-bar
// messages and durable notice cells. Renderers consume this value directly;
// they never classify authority or outcome by parsing user-visible prose.
type noticeKind uint8

const (
	noticeInfo noticeKind = iota
	noticeSuccess
	noticeGuidance
	noticeWarning
	noticeError
)

func noticeVisual(kind noticeKind) (glyph, color string) {
	switch kind {
	case noticeSuccess:
		return glyphCheck, "82"
	case noticeGuidance:
		return glyphArrowInto, "39"
	case noticeWarning:
		return glyphWarning, "214"
	case noticeError:
		return glyphCross, "203"
	default:
		return glyphBullet, "245"
	}
}

func noticeStyle(kind noticeKind) lipgloss.Style {
	_, color := noticeVisual(kind)
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(color))
	if kind == noticeInfo {
		style = style.Faint(true)
	}
	return style
}

// setStatusNotice installs one typed transient notice and returns its identity.
// Scheduled clears carry this id so an older timer cannot erase a newer notice.
func (m *uiModel) setStatusNotice(kind noticeKind, text string) uint64 {
	text = strings.TrimSpace(text)
	m.nextStatusNoticeID++
	m.statusNoticeID = m.nextStatusNoticeID
	m.statusNoticeKind = kind
	m.statusNoticeText = text
	m.statusMsg = text
	return m.statusNoticeID
}

func (m *uiModel) clearStatusNotice() {
	m.statusMsg = ""
	m.statusNoticeText = ""
	m.statusNoticeKind = noticeInfo
	m.statusNoticeID = 0
}

func clearStatusNoticeAfter(id uint64, delay time.Duration) tea.Cmd {
	if id == 0 || delay <= 0 {
		return nil
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return MsgClearStatus{NoticeID: id} })
}
