package cli

import (
	"fmt"
	uitheme "selfmind/internal/ui/theme"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

func (m *uiModel) transcriptVisibleHeight() int {
	if m == nil {
		return 1
	}
	inputH := 1
	if m.editor != nil {
		inputH = m.editor.PreferredHeight()
	}
	visibleH := m.height - inputH - 1 - m.composerGapHeight()
	if m.notificationBar(m.width) != "" {
		visibleH--
	}
	if m.migrationHintBar(m.width) != "" {
		visibleH--
	}
	if plan := m.activePlanBlock(m.width); plan != "" {
		visibleH -= lipgloss.Height(plan)
	}
	// Legacy mode draws the approval panel between transcript and composer, so
	// the viewport gives up that many rows.
	if m.approvalPrompt != nil {
		visibleH -= lipgloss.Height(m.approvalPrompt.View(m.width))
	}
	if visibleH < 1 {
		visibleH = 1
	}
	return visibleH
}

const maxActiveProcessRows = 10

func (m *uiModel) processRowBudget(width int) int {
	if m == nil || m.height <= 0 {
		return maxActiveProcessRows
	}
	reserved := 0
	if m.editor != nil {
		reserved += m.editor.PreferredHeight()
	} else {
		reserved++
	}
	reserved += lipgloss.Height(m.common.Styles.Status.Panel.Width(width).Render(m.statusLine()))
	if notice := m.notificationBar(width); notice != "" {
		reserved += lipgloss.Height(notice)
	}
	if hint := m.migrationHintBar(width); hint != "" {
		reserved += lipgloss.Height(hint)
	}
	if plan := m.activePlanBlock(width); plan != "" {
		reserved += lipgloss.Height(plan)
	}
	// The progress line has its own reserved slot above the composer, so tool
	// and process rows are budgeted around it. Reserving it here is what stops
	// a tall plan or Composer from squeezing the animation out of the layout.
	if activity := m.activityRowBlock(width); activity != "" {
		reserved += lipgloss.Height(activity)
	}
	if m.approvalPrompt != nil {
		reserved += lipgloss.Height(m.approvalPrompt.View(width))
	}
	reserved += m.composerGapHeight()
	if hint := m.composerHint(); hint != "" {
		reserved += lipgloss.Height(hint)
	}
	available := m.height - reserved
	if available < 0 {
		return 0
	}
	return min(available, maxActiveProcessRows)
}

// activePlanBlock gives the pinned plan its own visual band. The leading and
// trailing blank rows keep live progress distinct from both transcript events
// and the composer while remaining part of the measured active-region height.
func (m *uiModel) activePlanBlock(width int) string {
	if m == nil {
		return ""
	}
	plan := strings.TrimRight(renderPlanCellWithStyles(m.activePlanJSON, 0, width, newTranscriptStyles(m.common.Theme)), "\n")
	if strings.TrimSpace(plan) == "" {
		return ""
	}
	return "\n" + plan + "\n"
}

func (m *uiModel) composerGapHeight() int {
	if m == nil || m.height < 12 {
		return 0
	}
	// The live plan and approval prompt already provide a clear visual boundary
	// above the composer. Adding the generic spacer here would push useful
	// content upward and make a multi-step plan jump as it changes height. The
	// progress row carries its own trailing blank for the same reason, so a
	// second spacer would double the gap while a turn is running.
	if strings.TrimSpace(m.activePlanJSON) != "" || m.approvalPrompt != nil || m.activityRowBlock(m.width) != "" {
		return 0
	}
	inputH := 1
	if m.editor != nil {
		inputH = m.editor.PreferredHeight()
	}
	occupied := inputH + 1 // input area + status bar
	if m.notificationBar(m.width) != "" {
		occupied++
	}
	if m.migrationHintBar(m.width) != "" {
		occupied++
	}
	if m.height-occupied <= 6 {
		return 0
	}
	return 1
}

func (m *uiModel) viewModel() string {
	return m.viewActiveRegion()
}

// notificationBar renders a transient status notice (clipboard, mid-turn
// steering, cancellation) as a compact, left-aligned colored line with a
// leading glyph rather than a full-width grey slab. The slab read as leftover
// terminal output; a categorized accent line reads as a deliberate notice and
// makes the message kind (success / info / warning) obvious at a glance.
func (m *uiModel) notificationBar(width int) string {
	if width <= 0 || strings.TrimSpace(m.statusMsg) == "" {
		return ""
	}
	text := strings.TrimSpace(m.statusMsg)
	kind := noticeInfo
	if m.statusNoticeText == text {
		kind = m.statusNoticeKind
	}
	glyph, _ := noticeVisual(kind)
	body := glyph + " " + text
	return lipgloss.NewStyle().
		Padding(0, 1).
		Foreground(m.common.Theme.Color(noticeThemeRole(kind))).
		Bold(true).
		Render(truncateToWidth(body, width-2))
}

func noticeThemeRole(kind noticeKind) uitheme.Role {
	switch kind {
	case noticeSuccess:
		return uitheme.Success
	case noticeGuidance:
		return uitheme.Accent
	case noticeWarning:
		return uitheme.Warning
	case noticeError:
		return uitheme.Error
	default:
		return uitheme.TextSecondary
	}
}

func (m *uiModel) migrationHintBar(width int) string {
	if width <= 0 || m.migrationHint == "" {
		return ""
	}
	return lipgloss.NewStyle().
		Width(width).
		Padding(0, 1).
		Foreground(m.common.Theme.Color(uitheme.Warning)).
		Italic(true).
		Render(truncateToWidth("✨ "+m.migrationHint, width-2))
}

func (m *uiModel) renderHistoryContent(width int) string {
	if width < 20 {
		width = 20
	}
	if len(m.messages) == 0 {
		return "No conversation history yet."
	}
	var sb strings.Builder
	for i := range m.messages {
		msg := m.messages[i]
		var rendered string
		// Patches render with an effectively unbounded diff here (the inline
		// view bounds them; this overlay is where you see the whole change).
		if msg.ToolName == "patch" && !msg.IsError {
			if patch := patchArgOf(msg.ToolArgs); strings.TrimSpace(patch) != "" {
				rendered = renderPatchCellWithStyles(patch, msg.Duration, width, 1<<30, newTranscriptStyles(m.common.Theme))
			}
		}
		if rendered == "" {
			rendered = m.renderCell(msg, width)
		}
		if rendered = strings.TrimRight(rendered, "\n"); rendered == "" {
			continue
		}
		sb.WriteString(rendered + "\n\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (m *uiModel) renderHelpContent(width int) string {
	if width < 40 {
		width = 40
	}
	t := m.common.Theme
	title := lipgloss.NewStyle().Foreground(t.Color(uitheme.TextPrimary)).Bold(true)
	section := lipgloss.NewStyle().Foreground(t.Color(uitheme.TextPrimary)).Bold(true)
	keyStyle := lipgloss.NewStyle().Foreground(t.Color(uitheme.Warning)).Bold(true)
	cmdStyle := lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary))
	muted := lipgloss.NewStyle().Foreground(t.Color(uitheme.TextDecorative))

	lines := []string{
		"",
		title.Render("SelfMind help"),
		muted.Render("Send tasks, inspect code, run tools, and manage what SelfMind learns."),
		"",
		section.Render("Keyboard shortcuts:"),
	}

	for _, row := range shortcutHelpRows {
		lines = append(lines, renderHelpRow(row.Left, row.Right, keyStyle, descStyle, width))
	}

	lines = append(lines, "", section.Render("Slash commands:"))
	for _, cmd := range slashHelpMetas() {
		lines = append(lines, renderHelpRow(cmd.Usage, cmd.Description, cmdStyle, descStyle, width))
	}

	lines = append(lines, "", muted.Render("Press q or Esc to close this page."))
	return strings.Join(lines, "\n")
}

func (m *uiModel) statusLine() string {
	st := m.common.Styles
	header := m.displayModelName()
	if meta := strings.TrimSpace(m.modelMeta); meta != "" {
		header += " " + meta
	}
	// The directory segment shows where the NEXT message actually runs: the
	// session workspace override when one is set (/workspace <n>), else the
	// launch cwd. Rendering cwd while an override is active lied about the
	// execution root (defect: stale status bar after /workspace).
	dir := currentWorkingDir()
	if m.workspaceOverridePath != "" {
		if m.workspaceOverrideName != "" {
			dir = m.workspaceOverrideName + ":" + m.workspaceOverridePath
		} else {
			dir = m.workspaceOverridePath
		}
	}

	parts := []string{
		st.Status.Value.Render(header),
		st.Status.Value.Render(dir),
		st.Status.Label.Render(formatUsageSession(m.runTokens, m.totalTokens, m.tokenLimit)),
	}
	if m.modelChangePhase != "" {
		phase := strings.ReplaceAll(string(m.modelChangePhase), "_", " ")
		if !m.modelChangePhaseAt.IsZero() {
			phase = fmt.Sprintf("%s %.0fs", phase, time.Since(m.modelChangePhaseAt).Seconds())
		}
		parts = append(parts, st.Status.Warning.Render("model change: "+phase))
	}

	state := m.runStatus
	stateStyle := st.Status.Good
	switch {
	case m.approvalFlowActive() || len(m.delayedApprovals) > 0:
		// A pending approval is a distinct state — the run is paused on the
		// user, not "working", and definitely not "ready".
		state = "⏸ waiting approval"
		stateStyle = st.Status.Warning
	case m.backgroundDaemonRunActive():
		if m.backgroundWatchID != "" {
			state = "background watcher finalizing"
		} else if origin := strings.TrimSpace(m.backgroundOrigin); origin != "" {
			state = "background " + origin
		} else {
			state = "background task"
		}
		stateStyle = st.Status.Warning
	case m.daemonRunActive && !m.daemonRunStarted.IsZero():
		state = fmt.Sprintf("working %.1fs", time.Since(m.daemonRunStarted).Seconds())
	case m.localRequestActive && !m.thinkingStart.IsZero():
		state = fmt.Sprintf("working %.1fs", time.Since(m.thinkingStart).Seconds())
	case m.runStatus == "queued":
		count := m.queuedCount
		if count < 1 {
			count = 1
		}
		state = fmt.Sprintf("queued %d", count)
	}
	parts = append(parts, stateStyle.Render(state))

	// Always show the effective approval mode so the user can see it at a glance.
	// In client mode approvalMode is "" (defers to the daemon); the effective
	// value comes from the persisted mode learned via the startup digest, falling
	// back to on-request when unknown.
	parts = append(parts, st.Status.Label.Render("mode:"+m.effectiveApprovalMode()))

	ctrlHint := "Ctrl+C exit"
	if m.localRequestActive {
		ctrlHint = "Ctrl+C cancel"
	} else if strings.TrimSpace(m.editor.Value()) != "" {
		ctrlHint = "Ctrl+C clear"
	}
	parts = append(parts, st.Status.Label.Render(ctrlHint), st.Status.Label.Render("/help"))

	return strings.Join(parts, " · ")
}

func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	limit := width - 3
	var sb strings.Builder
	used := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if used+rw > limit {
			break
		}
		sb.WriteRune(r)
		used += rw
	}
	return sb.String() + "..."
}
