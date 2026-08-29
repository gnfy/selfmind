package cli

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/ui/layout"
)

// maxHistoryWindow bounds the in-memory message log so a marathon session does
// not grow unboundedly. Committed cells already live in terminal scrollback (or
// can be re-fetched from control.db later); only the oldest committed messages
// are evicted, never an in-flight one. Generous so eviction is rare.
const maxHistoryWindow = 2000

// trimHistoryWindow drops the oldest committed messages once the in-memory log
// exceeds the cap. It only removes a committed prefix, so active/in-flight cells
// and the indices handlers compute within a single Update are unaffected.
func (m *uiModel) trimHistoryWindow() {
	if len(m.messages) <= maxHistoryWindow {
		return
	}
	drop := len(m.messages) - maxHistoryWindow
	i := 0
	for i < drop && i < len(m.messages) && m.messages[i].Committed {
		i++
	}
	if i > 0 {
		m.messages = append(m.messages[:0:0], m.messages[i:]...)
	}
}

// composerHintStyle dims the mid-turn guidance hint shown above the input.
var composerHintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Italic(true)

// composerHint returns a hint shown above the input WHILE a run is active and
// the user has typed something — so they know Enter will inject the text as
// guidance into the running task (not start a new turn). Empty otherwise.
func (m *uiModel) composerHint() string {
	if m.steerCh == nil || (!m.thinking && m.toolExecuting == "") {
		return ""
	}
	if strings.TrimSpace(m.editor.Value()) == "" {
		return ""
	}
	return composerHintStyle.Render(glyphArrowInto + " Enter sends as guidance to the running task")
}

// clearHybridScreen wipes the terminal (screen + visible scrollback) and
// re-shows the startup card, so /clear and ctrl+l feel like a clean slate even
// though committed history otherwise lives in immutable scrollback. ClearScreen
// runs before the card print, so the card survives.
func (m *uiModel) clearHybridScreen() tea.Cmd {
	cmds := []tea.Cmd{tea.ClearScreen}
	if m.width > 0 {
		if card := strings.TrimRight(strings.Join(m.renderStartupCard(m.width), "\n"), "\n"); card != "" {
			cmds = append(cmds, tea.Println(card))
		}
	}
	return tea.Sequence(cmds...)
}

// handleCopyLast copies the most recent successful assistant response to the
// clipboard. It sources content from m.messages (not the rendered buffer), so
// it works regardless of whether that cell has scrolled into native scrollback.
func (m *uiModel) handleCopyLast() tea.Cmd {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Role != "assistant" || msg.IsError || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		if err := copyToClipboard(msg.Content); err != nil {
			m.setStatusNotice(noticeWarning, fmt.Sprintf("Copy failed: %v", err))
		} else {
			m.setStatusNotice(noticeSuccess, "Copied last response to clipboard.")
		}
		return nil
	}
	m.setStatusNotice(noticeWarning, "No response to copy yet.")
	return nil
}

// Terminal-first hybrid rendering (see docs/tui-terminal-first-hybrid.md).
//
// The transcript is NOT held in a viewport and re-rendered every
// frame. Instead, each cell is committed to the terminal's native scrollback
// (via the bubbletea Program's Println) the moment it finalizes, and View()
// renders only the small active region (in-progress cells, live stream,
// spinner, input, status). The terminal then owns scroll, selection, copy, and
// long-history performance. Committed cells are immutable.

// commitWidth is the wrap width used when rendering a cell into scrollback.
func (m *uiModel) commitWidth() int {
	w := m.width
	if w <= 0 {
		w = 80
	}
	return w
}

// commit queues a finalized message to be printed into native scrollback and
// marks it immutable so the active region stops rendering it. It is a no-op if
// the message is already committed.
//
// IMPORTANT: this must NOT call Program.Println directly. Program.Println sends
// on the program's message channel and blocks until the loop consumes it; doing
// that from inside Update (the same goroutine that drives the loop) deadlocks —
// the symptom was the TUI freezing on the first submit. Instead we accumulate
// the rendered lines and the Update wrapper flushes them as tea.Println Cmds.
func (m *uiModel) commit(msg *ChatMessage) {
	if msg == nil || msg.Committed {
		return
	}
	msg.Committed = true
	rendered := prepareTerminalCell(renderCell(*msg, m.commitWidth()), m.commitWidth())
	if rendered != "" {
		m.pendingPrintln = append(m.pendingPrintln, rendered)
	}
}

// flushPendingPrintln converts queued committed cells into ordered tea.Println
// commands and clears the queue. Called once per Update (by the wrapper), so
// the prints are emitted by the program loop after Update returns — never
// synchronously from within it. cmd is the handler's original command, which
// runs after the prints so the committed cell appears before any follow-up.
func (m *uiModel) flushPendingPrintln(cmd tea.Cmd) tea.Cmd {
	if len(m.pendingPrintln) == 0 {
		return cmd
	}
	cmds := make([]tea.Cmd, 0, len(m.pendingPrintln)+1)
	for _, line := range m.pendingPrintln {
		cmds = append(cmds, tea.Println(line))
	}
	m.pendingPrintln = nil
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Sequence(cmds...)
}

// renderActiveBlock renders the not-yet-committed tail: in-progress tool cells,
// the live assistant stream, and the thinking spinner. This is the only
// transcript content the hybrid View draws each frame, so its cost is bounded
// by what is currently in flight, not by history length.
func (m *uiModel) renderActiveBlock(width int) string {
	st := m.common.Styles
	var lines []string
	for i := range m.messages {
		if m.messages[i].Committed {
			continue
		}
		rendered := prepareTerminalCell(renderCell(m.messages[i], width), width)
		if rendered == "" {
			continue
		}
		lines = append(lines, strings.Split(rendered, "\n")...)
	}
	if strings.TrimSpace(m.liveStreamContent) != "" {
		phase := m.liveStreamPhase
		if phase == llm.AssistantPhaseUnspecified {
			// Until the kernel reaches a tool/final boundary this is a mutable
			// preview, not a canonical answer. Render it as subordinate progress;
			// MsgAgentDone will commit the authoritative final with its gutter.
			phase = llm.AssistantPhaseCommentary
		}
		rendered := prepareTerminalCell(renderAssistantMessagePhase(stripANSI(m.liveStreamContent), width, phase), width)
		if rendered != "" {
			lines = append(lines, strings.Split(rendered, "\n")...)
		}
	}
	// While the approval panel is up, the run is paused on the user: the
	// spinner/activity line ("Preparing to run <tool>…") would be noise next to
	// the panel, so it is suppressed until the decision resumes the run.
	if m.thinking && m.approvalPrompt == nil {
		spinnerView := m.spinner.View()
		dots := strings.Repeat(".", (m.thinkingDots%3)+1)
		label := strings.TrimSpace(m.activityText)
		if label == "" {
			label = "Working"
		}
		lines = append(lines, st.Chat.Thinking.Render(spinnerView+" "+label+dots))
	}
	return strings.Join(lines, "\n")
}

// viewActiveRegion is the hybrid-mode View: only the active region, pinned at
// the bottom of the terminal. Finalized history lives in scrollback above it.
func (m *uiModel) viewActiveRegion() string {
	if m.modelManager != nil {
		return m.modelManager.View()
	}
	if m.pager != nil {
		return m.pager.View()
	}
	mainW := m.width
	st := m.common.Styles

	m.editor.SetCursorVisible(m.cursorVisible)
	inputH := m.editor.PreferredHeight()
	inputArea := m.editor.Draw(layout.Rect{W: m.width, H: inputH})
	statusBar := st.Status.Panel.Width(m.width).Render(m.statusLine())
	notification := m.notificationBar(mainW)
	migrationHint := m.migrationHintBar(mainW)

	var parts []string
	if active := m.renderActiveBlock(mainW); strings.TrimSpace(active) != "" {
		parts = append(parts, active)
	}
	// Transient approval dialog: active-region content per the hybrid contract
	// (docs/tui-terminal-first-hybrid.md §3) — it must never scroll away into
	// history while undecided.
	if notification != "" {
		parts = append(parts, notification)
	}
	if migrationHint != "" {
		parts = append(parts, migrationHint)
	}
	// Keep the latest plan in the mutable bottom region, immediately above any
	// blocking prompt/composer. It updates in place and never pollutes native
	// terminal scrollback with stale snapshots.
	if plan := m.activePlanBlock(mainW); plan != "" {
		parts = append(parts, plan)
	}
	// A blocking approval stays closest to the composer so the next expected
	// user action remains obvious even when a plan is visible.
	if m.approvalPrompt != nil {
		parts = append(parts, m.approvalPrompt.View(mainW))
	}
	if gapH := m.composerGapHeight(); gapH > 0 {
		parts = append(parts, lipgloss.NewStyle().Width(m.width).Height(gapH).Render(""))
	}
	if hint := m.composerHint(); hint != "" {
		parts = append(parts, hint)
	}
	parts = append(parts, inputArea, statusBar)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}
