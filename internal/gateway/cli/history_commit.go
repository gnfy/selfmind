package cli

import (
	"fmt"
	"strings"
	"time"

	uitheme "selfmind/internal/ui/theme"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	return lipgloss.NewStyle().
		Foreground(m.common.Theme.Color(uitheme.Accent)).
		Italic(true).
		Render(glyphArrowInto + " Enter sends as guidance to the running task")
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
	rendered := prepareTerminalCell(m.renderCell(*msg, m.commitWidth()), m.commitWidth())
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

// renderActiveBlock renders the process surface and its single working spinner.
// This is the only
// transcript content the hybrid View draws each frame, so its cost is bounded
// by what is currently in flight, not by history length.
func (m *uiModel) renderActiveBlock(width int) string {
	var lines []string
	if rendered := m.processState().RenderWithTheme(processViewport{width: width, maxRows: m.processRowBudget(width)}, m.common.Theme); strings.TrimSpace(rendered) != "" {
		lines = append(lines, strings.Split(rendered, "\n")...)
	}
	return strings.Join(lines, "\n")
}

// animatingTurn reports whether this terminal owns work that is in flight right
// now. It is deliberately the union of every live phase — provider wait, tool
// execution, the gap between phases, an outstanding synchronous request, and an
// owned daemon run — because the person only needs to know that their work is
// moving, and a gap between two of those phases is exactly when the animation
// used to vanish.
func (m *uiModel) animatingTurn() bool {
	if m == nil {
		return false
	}
	switch {
	case m.waitingForModel, m.thinking, m.localRequestActive:
		return true
	case strings.TrimSpace(m.toolExecuting) != "":
		return true
	case m.daemonRunActive && m.daemonRunOwned:
		return true
	}
	return m.processState().HasRunningTools()
}

// activityRow is the run's one progress line, and it owns a fixed slot: below
// the plan and directly above the composer. Previously the spinner lived at the
// TOP of the active region inside the process-row budget, so it moved whenever
// tool rows accumulated or the plan grew, and the budget could squeeze it out
// entirely — the animation appeared to be lost mid-run. As its own reserved row
// the position never changes for the life of the turn.
//
// A blocking approval owns the next action instead, so the row yields to the
// panel; the run is paused on the person, not progressing.
func (m *uiModel) activityRow(width int) string {
	if m == nil || width <= 0 || m.approvalPrompt != nil || !m.animatingTurn() {
		return ""
	}
	// Name only what is actually known. Without a structured phase the run is
	// moving but its stage is not established, so the row says "Working" rather
	// than claiming a provider wait: a resumed approval must not be reported as
	// a model wait that never started.
	label := strings.TrimSpace(m.activityText)
	if label == "" {
		switch {
		case strings.TrimSpace(m.toolExecuting) != "":
			label = "Running " + strings.TrimSpace(m.toolExecuting)
		case m.waitingForModel:
			label = "Waiting for the model"
		default:
			label = "Working"
		}
	}
	started := m.thinkingStart
	if started.IsZero() {
		started = m.daemonRunStarted
	}
	line := trimActivityElapsed(label)
	if !started.IsZero() {
		elapsed := int(time.Since(started).Seconds())
		if elapsed < 1 {
			elapsed = 1
		}
		line = fmt.Sprintf("%s (%ds)", line, elapsed)
	}
	chat := m.common.Styles.Chat
	return chat.ProgressGlyph.Render(m.spinner.View()) + " " + chat.ProgressLabel.Render(truncateToWidth(line, width-2))
}

// activityRowBlock gives the progress line one blank row above and one below so
// it reads as its own band. Without the trailing blank the row sat flush against
// the Composer while the plan above it kept its gap, which looked like part of
// the input frame. The plan block already ends with a blank row, so the leading
// blank is added only when no plan precedes the line.
func (m *uiModel) activityRowBlock(width int) string {
	row := m.activityRow(width)
	if row == "" {
		return ""
	}
	if strings.TrimSpace(m.activePlanBlock(width)) != "" {
		return row + "\n"
	}
	return "\n" + row + "\n"
}

// viewActiveRegion is the hybrid-mode View: only the active region, pinned at
// the bottom of the terminal. Finalized history lives in scrollback above it.
func (m *uiModel) viewActiveRegion() string {
	// While an apply is in flight the wizard has nothing left to ask, and its
	// last screen still carries a stale per-route "validating…" line and an
	// "Enter continue" footer. Fall through to the ordinary active region so
	// the animated progress row reports the daemon round trip instead.
	if m.modelManager != nil && !m.modelApplying && m.approvalPrompt == nil && m.workspaceTrustPrompt == nil {
		return m.modelManager.View()
	}
	if m.pager != nil && m.approvalPrompt == nil && m.workspaceTrustPrompt == nil {
		return m.pager.View()
	}
	mainW := m.width
	st := m.common.Styles

	m.editor.SetCursorVisible(m.cursorVisible)
	m.editor.SetLayout(m.width, m.height)
	inputH := m.editor.PreferredHeight()
	inputArea := m.editor.Draw(layout.Rect{W: m.width, H: inputH})
	statusBar := st.Status.Panel.Width(m.width).Render(m.statusLine())
	notification := m.notificationBar(mainW)
	migrationHint := m.migrationHintBar(mainW)

	var parts []string
	if active := m.renderActiveBlock(mainW); strings.TrimSpace(active) != "" {
		parts = append(parts, active)
	}
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
	// The progress line sits between the plan and the composer: the plan keeps
	// its established position, and the animation keeps one fixed slot that no
	// amount of tool output or plan growth can move or squeeze away.
	if activity := m.activityRowBlock(mainW); activity != "" {
		parts = append(parts, activity)
	}
	// A blocking approval stays closest to the composer so the next expected
	// user action remains obvious. It temporarily preempts a pager/model overlay
	// without hiding the active process, Plan, draft, or status context.
	if m.approvalPrompt != nil {
		parts = append(parts, m.approvalPrompt.View(mainW))
	}
	// The trust question sits in the same slot but yields to an approval: a run
	// blocked on the person is the more urgent of the two, and only one question
	// may own the keyboard at a time.
	if m.approvalPrompt == nil && m.workspaceTrustPrompt != nil {
		parts = append(parts, m.workspaceTrustPrompt.View(mainW))
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
