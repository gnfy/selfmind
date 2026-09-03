package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/buildinfo"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/ui/components"
	uitheme "selfmind/internal/ui/theme"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// cellRenderer turns one ChatMessage into its rendered (pre-split) string for a
// given width. The registry lets new transcript cell kinds (e.g. approval
// cards, plan widgets) plug in without touching the shared renderers — the
// extensibility hook for later phases.
type cellRenderer func(msg ChatMessage, width int) string

type transcriptStyles struct {
	markdown                                       components.MarkdownRenderer
	startupBorder, startupBrand, startupLabel      lipgloss.Style
	startupValue                                   lipgloss.Style
	startupDescription, startupSubtle              lipgloss.Style
	startupCommand                                 lipgloss.Style
	userBoundary, userLabel, userText              lipgloss.Style
	currentMarker, finalBullet, narrationMarker    lipgloss.Style
	toolEvidence, toolBulletRun                    lipgloss.Style
	toolBulletOK, toolBulletErr, toolBulletDim     lipgloss.Style
	toolAction, diffAdd, diffDel, diffContext      lipgloss.Style
	planDone, planActive, planPending, planExplain lipgloss.Style
	planHeader, planSecondary                      lipgloss.Style
}

func newTranscriptStyles(t uitheme.Theme) transcriptStyles {
	return transcriptStyles{
		markdown:           components.NewMarkdownRenderer(t),
		startupBorder:      lipgloss.NewStyle().Foreground(t.Color(uitheme.BorderMuted)),
		startupBrand:       lipgloss.NewStyle().Foreground(t.Color(uitheme.Brand)).Bold(true),
		startupLabel:       lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)).Bold(true),
		startupValue:       lipgloss.NewStyle().Foreground(t.Color(uitheme.TextPrimary)),
		startupDescription: lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)),
		startupSubtle:      lipgloss.NewStyle().Foreground(t.Color(uitheme.TextDecorative)),
		startupCommand:     lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)),
		userBoundary:       lipgloss.NewStyle().Foreground(t.Color(uitheme.BorderMuted)),
		userLabel:          lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)).Bold(true),
		userText:           lipgloss.NewStyle(),
		currentMarker:      lipgloss.NewStyle().Bold(true).Foreground(t.Color(uitheme.Accent)),
		finalBullet:        lipgloss.NewStyle().Foreground(t.Color(uitheme.TextDecorative)),
		narrationMarker:    lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)),
		toolEvidence:       lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)),
		toolBulletRun:      lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)),
		toolBulletOK:       lipgloss.NewStyle().Foreground(t.Color(uitheme.Success)),
		toolBulletErr:      lipgloss.NewStyle().Foreground(t.Color(uitheme.Error)),
		toolBulletDim:      lipgloss.NewStyle().Foreground(t.Color(uitheme.TextDecorative)),
		toolAction:         lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)).Bold(true),
		diffAdd:            lipgloss.NewStyle().Foreground(t.Color(uitheme.Success)),
		diffDel:            lipgloss.NewStyle().Foreground(t.Color(uitheme.Error)),
		diffContext:        lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)),
		planDone:           lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)).Strikethrough(true),
		planActive:         lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)).Bold(true),
		planPending:        lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)),
		planExplain:        lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)).Italic(true),
		planHeader:         lipgloss.NewStyle().Bold(true),
		planSecondary:      lipgloss.NewStyle().Foreground(t.Color(uitheme.TextDecorative)),
	}
}

// renderCell dispatches a message to its registered renderer. Unknown roles
// render to empty (matching the previous switch default).
func renderCell(msg ChatMessage, width int) string {
	return renderCellWithTheme(msg, width, uitheme.Default())
}

func renderCellWithTheme(msg ChatMessage, width int, t uitheme.Theme) string {
	styles := newTranscriptStyles(t)
	switch msg.Role {
	case "user":
		return renderUserMessageWithStyles(stripANSI(msg.Content), width, styles)
	case "assistant":
		return renderAssistantMessagePhaseWithStyles(stripANSI(msg.Content), width, msg.AssistantPhase, styles)
	case "tool":
		return renderProcessToolMessageWithStyles(msg, width, styles)
	case "system":
		return renderSystemMessage(stripANSI(msg.Content), width)
	case "digest":
		return renderDigestMessageWithStyles(stripANSI(msg.Content), width, styles)
	case "notice":
		return renderNoticeMessageWithTheme(stripANSI(msg.Content), msg.NoticeKind, width, t)
	default:
		return ""
	}
}

func (m *uiModel) renderCell(msg ChatMessage, width int) string {
	if m != nil && m.common != nil {
		return renderCellWithTheme(msg, width, m.common.Theme)
	}
	return renderCell(msg, width)
}

const (
	glyphBullet  = "\u2022"
	glyphCorner  = "\u2514" // \u2514 tree connector (codex-style; used as `glyphCorner + " "`)
	glyphChevron = "\u203a"
	// Notification glyphs (see noticeVisual). Kept to widely-supported
	// code points so they render across terminals without width surprises.
	glyphCheck     = "\u2713" // checkmark: success
	glyphArrowInto = "\u21b3" // down-right arrow: steering injected into the run
	glyphWarning   = "\u26a0" // warning sign: recoverable problem
	glyphCross     = "\u2717" // ballot X: cancelled/aborted
	glyphDot       = " · "
)

func (m *uiModel) renderStartupCard(width int) []string {
	styles := newTranscriptStyles(m.common.Theme)
	if width < 24 {
		return []string{m.common.Styles.Welcome}
	}

	modelName := strings.TrimSpace(m.modelName)
	if modelName == "" {
		modelName = strings.TrimSpace(m.providerName)
	}
	if modelName == "" {
		modelName = "active"
	}
	providerName := strings.TrimSpace(m.providerName)
	version := strings.TrimSpace(buildinfo.Version)
	if version == "" {
		version = "dev"
	}
	mainValue := renderStartupModelValue(modelName, providerName, m.modelManagerStatus.PrimaryReasoning, styles)

	lines := []string{
		styles.startupBrand.Render(">_ SelfMind") + styles.startupSubtle.Render("  "+version),
		styles.startupBorder.Render(strings.Repeat("─", width)),
	}
	lines = append(lines, renderStartupDataLines("MAIN", mainValue, width, styles.startupCommand.Render("/model"), styles)...)
	lines = append(lines, renderStartupDescriptionLines("Handles requests, planning, tool use, and final answers.", width, styles)...)
	lines = append(lines, "")

	backgroundValue := ""
	backgroundStatusKnown := strings.TrimSpace(m.modelManagerStatus.ConfiguredBackground) != "" ||
		strings.TrimSpace(m.modelManagerStatus.BackgroundProvider) != "" || strings.TrimSpace(m.modelManagerStatus.BackgroundModel) != ""
	if backgroundStatusKnown && !m.modelManagerStatus.BackgroundEnabled {
		backgroundValue = styles.startupValue.Render("disabled")
	} else if m.modelManagerStatus.BackgroundProvider != "" || m.modelManagerStatus.BackgroundModel != "" {
		backgroundValue = renderStartupModelValue(
			m.modelManagerStatus.BackgroundModel,
			m.modelManagerStatus.BackgroundProvider,
			m.modelManagerStatus.BackgroundReasoning,
			styles,
		)
	} else if background := strings.TrimSpace(m.backgroundModelName); background != "" {
		backgroundValue = styles.startupValue.Render(background)
	} else {
		backgroundValue = styles.startupValue.Render("uses Main")
	}
	lines = append(lines, renderStartupDataLines("BACKGROUND", backgroundValue, width, "", styles)...)
	lines = append(lines, renderStartupDescriptionLines("Default for maintenance and roles without an explicit override.", width, styles)...)
	lines = append(lines, "")

	roleLabel := "ROLES"
	for _, route := range modelchange.ManagedRoleRoutes() {
		role, ok := m.modelManagerStatus.RoleOverrides[string(route)]
		if !ok || (strings.TrimSpace(role.Provider) == "" && strings.TrimSpace(role.Model) == "") {
			continue
		}
		roleValue := styles.startupValue.Render(string(route)) + styles.startupSubtle.Render(" · ") +
			renderStartupModelValue(role.Model, role.Provider, role.Reasoning, styles)
		lines = append(lines, renderStartupDataLines(roleLabel, roleValue, width, "", styles)...)
		lines = append(lines, renderStartupDescriptionLines(startupRoleDescription(string(route)), width, styles)...)
		roleLabel = ""
	}
	if roleLabel == "" {
		lines = append(lines, "")
	}
	workspaceValue := currentWorkingDir()
	if strings.TrimSpace(m.workspaceOverridePath) != "" {
		workspaceValue = m.workspaceOverridePath
	}
	lines = append(lines, renderStartupDataLines("WORKSPACE", styles.startupValue.Render(workspaceValue), width, "", styles)...)
	lines = append(lines,
		styles.startupBorder.Render(strings.Repeat("─", width)),
		"",
	)
	if m.firstTaskPending {
		lines = append(lines,
			styles.startupValue.Render("Try: Inspect this workspace and explain how it is structured."),
			styles.startupSubtle.Render("Do not modify files."),
			"",
		)
	} else {
		lines = append(lines,
			styles.startupValue.Render("Tip: Tell SelfMind what to inspect, change, test, or remember."),
			"",
		)
	}
	return lines
}

func renderStartupModelValue(model, provider, reasoning string, styles transcriptStyles) string {
	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)
	reasoning = strings.TrimSpace(reasoning)
	if model == "" {
		model = provider
		provider = ""
	}
	if model == "" {
		model = "active"
	}
	parts := []string{styles.startupValue.Render(model)}
	if provider != "" && !strings.EqualFold(provider, model) && !strings.EqualFold(provider, "active") {
		parts = append(parts, styles.startupValue.Render(provider))
	}
	if reasoning != "" && !strings.EqualFold(reasoning, "auto") {
		parts = append(parts, styles.startupValue.Render(reasoning))
	}
	return strings.Join(parts, styles.startupSubtle.Render(" · "))
}

func startupRoleDescription(role string) string {
	switch strings.TrimSpace(role) {
	case "fast_classifier":
		return "Makes fast, bounded decisions for approval triage."
	case "memory_extract":
		return "Extracts and consolidates durable user preferences."
	case "background_review":
		return "Reviews completed work and supports maintenance fallback."
	case "skill_curator":
		return "Creates or repairs Skills from verified reusable evidence."
	case "semantic_recall":
		return "Expands recall queries to find relevant memory and work history."
	case "summarizer":
		return "Compacts long context while preserving decisions and unresolved work."
	default:
		return ""
	}
}

func renderUserMessageWithStyles(content string, width int, styles transcriptStyles) string {
	content = strings.TrimRight(content, "\n")
	if width < 8 {
		width = 8
	}
	const prefix = "── "
	const label = "YOUR REQUEST"
	used := runewidth.StringWidth(prefix) + runewidth.StringWidth(label) + 1
	top := styles.userBoundary.Render(prefix) + styles.userLabel.Render(label) +
		styles.userBoundary.Render(" "+strings.Repeat("─", max(0, width-used)))
	bottom := styles.userBoundary.Render(strings.Repeat("─", width))
	body := styles.userText.Render(wrapText(content, width))
	return "\n" + strings.Join([]string{top, body, bottom}, "\n")
}

// currentMarkerStyle highlights the gateway's "← current" marker (e.g. in the
// /workspaces list) so the active selection stands out — same cyan family as
// the command hints. Applied at render time only; the gateway text stays plain
// for IM surfaces.
func renderStartupDataLines(label, value string, width int, suffix string, styles transcriptStyles) []string {
	const labelWidth = 12
	if width <= labelWidth {
		return []string{styles.startupLabel.Render(label), value}
	}

	labelText := label + strings.Repeat(" ", max(0, labelWidth-runewidth.StringWidth(label)))
	indent := strings.Repeat(" ", labelWidth)
	valueWidth := width - labelWidth
	valueDisplayWidth := runewidth.StringWidth(stripANSI(value))
	suffixWidth := runewidth.StringWidth(stripANSI(suffix))
	if suffix == "" || valueDisplayWidth+1+suffixWidth <= valueWidth {
		line := styles.startupLabel.Render(labelText) + value
		if suffix != "" {
			line += strings.Repeat(" ", valueWidth-valueDisplayWidth-suffixWidth) + suffix
		}
		return []string{line}
	}

	wrapped := strings.Split(wrapText(value, valueWidth), "\n")
	lines := make([]string, 0, len(wrapped)+1)
	for i, part := range wrapped {
		prefix := indent
		if i == 0 {
			prefix = styles.startupLabel.Render(labelText)
		}
		lines = append(lines, prefix+part)
	}

	last := len(lines) - 1
	lastWidth := runewidth.StringWidth(stripANSI(wrapped[len(wrapped)-1]))
	if lastWidth+1+suffixWidth <= valueWidth {
		lines[last] += strings.Repeat(" ", valueWidth-lastWidth-suffixWidth) + suffix
	} else {
		lines = append(lines, indent+strings.Repeat(" ", max(0, valueWidth-suffixWidth))+suffix)
	}
	return lines
}

func renderStartupDescriptionLines(description string, width int, styles transcriptStyles) []string {
	const labelWidth = 12
	description = strings.TrimSpace(description)
	if description == "" {
		return nil
	}
	if width <= labelWidth {
		wrapped := strings.Split(wrapText(description, max(1, width)), "\n")
		for i := range wrapped {
			wrapped[i] = styles.startupDescription.Render(wrapped[i])
		}
		return wrapped
	}
	indent := strings.Repeat(" ", labelWidth)
	wrapped := strings.Split(wrapText(description, width-labelWidth), "\n")
	for i := range wrapped {
		wrapped[i] = indent + styles.startupDescription.Render(wrapped[i])
	}
	return wrapped
}

func renderAssistantStreamPreviewWithStyles(content string, width int, phase llm.AssistantPhase, styles transcriptStyles) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	stable, tail := splitStableMarkdownPrefix(content)
	blocks := make([]string, 0, 2)
	if strings.TrimSpace(stable) != "" {
		if rendered := strings.TrimSpace(styles.markdown.Render(stable, max(8, width-2))); rendered != "" {
			blocks = append(blocks, rendered)
		}
	}
	if strings.TrimSpace(tail) != "" {
		if rendered := renderPlainAssistantTail(tail, max(8, width-2)); rendered != "" {
			blocks = append(blocks, rendered)
		}
	}
	body := strings.Join(blocks, "\n\n")
	if body == "" {
		return ""
	}
	return renderAssistantBodyWithStyles(body, phase, styles)
}

func renderPlainAssistantTail(content string, width int) string {
	content = strings.TrimSpace(sanitizeTerminalText(content))
	if content == "" {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		if line == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, strings.Split(wrapText(line, width), "\n")...)
	}
	return strings.Join(lines, "\n")
}

func renderAssistantBodyWithStyles(body string, phase llm.AssistantPhase, styles transcriptStyles) string {
	lines := strings.Split(body, "\n")
	firstContentLine := true
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		if strings.Contains(line, "← current") {
			line = strings.ReplaceAll(line, "← current", styles.currentMarker.Render("← current"))
		}
		switch phase {
		case llm.AssistantPhaseCommentary:
			if firstContentLine {
				lines[i] = styles.narrationMarker.Render(glyphChevron+" ") + line
				firstContentLine = false
			} else {
				lines[i] = "  " + line
			}
		case llm.AssistantPhaseFinalAnswer:
			if firstContentLine {
				lines[i] = styles.finalBullet.Render(glyphBullet+" ") + line
				firstContentLine = false
			} else {
				lines[i] = "  " + line
			}
		default:
			lines[i] = "  " + line
		}
	}
	return "\n" + strings.Join(lines, "\n")
}

func renderAssistantMessagePhaseWithStyles(content string, width int, phase llm.AssistantPhase, styles transcriptStyles) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if phase == llm.AssistantPhaseUnspecified {
		phase = llm.AssistantPhaseFinalAnswer
	}
	body := strings.TrimRight(styles.markdown.Render(content, max(8, width-2)), "\n")
	if body == "" {
		return ""
	}
	return renderAssistantBodyWithStyles(body, phase, styles)
}

const glyphBulletHollow = "◦"

func isCommandTool(label string) bool {
	switch label {
	case "terminal", "execute_command", "shell":
		return true
	}
	return false
}

// toolSemanticActionStyle colors only the action verb. The target/evidence uses
// a readable muted color, while the bullet independently retains runtime state:
// green success, red failure, and dim running/non-command. This mirrors Codex's
// cyan exploration verbs and makes run/write/lifecycle work equally scannable.
func toolSemanticActionStyleWithStyles(_ string, styles transcriptStyles) lipgloss.Style {
	return styles.toolAction
}

func styleToolActionWithStyles(label, action string, styles transcriptStyles) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	verb, rest, found := strings.Cut(action, " ")
	styled := toolSemanticActionStyleWithStyles(label, styles).Render(verb)
	if found && rest != "" {
		styled += " " + styles.toolEvidence.Render(rest)
	}
	return styled
}

// toolHeaderLine renders the codex-style cell header: a status bullet (◦ dim
// while running, • green on command success, • red on failure, • dim otherwise)
// followed by the bold action title.
func toolHeaderLineWithStyles(label, action string, running, isErr, isCommand bool, duration float64, width int, styles transcriptStyles) string {
	var bullet string
	switch {
	case running:
		bullet = styles.toolBulletRun.Render(glyphBulletHollow)
	case isErr:
		bullet = styles.toolBulletErr.Render(glyphBullet)
	case isCommand:
		bullet = styles.toolBulletOK.Render(glyphBullet)
	default:
		bullet = styles.toolBulletDim.Render(glyphBullet)
	}
	action = strings.TrimSpace(sanitizeTerminalText(action))
	suffix := ""
	if duration > 0 {
		suffix = fmt.Sprintf(" %.1fs", duration)
	}
	available := width - 2 - runewidth.StringWidth(suffix)
	if available < 12 {
		available = 12
	}
	rows := physicalDisplayLines(action, available)
	if len(rows) == 0 {
		rows = []string{"tool"}
	}
	if len(rows) > maxToolHeaderRows {
		rows = rows[:maxToolHeaderRows]
		rows[1] = truncateToWidth(rows[1], max(4, available-3)) + "..."
	}
	var sb strings.Builder
	sb.WriteString(bullet + " " + styleToolActionWithStyles(label, rows[0], styles) + styles.toolBulletDim.Render(suffix))
	for _, row := range rows[1:] {
		sb.WriteString("\n  " + styles.toolEvidence.Render(row))
	}
	return sb.String()
}

func renderProcessToolMessageWithStyles(msg ChatMessage, width int, styles transcriptStyles) string {
	if msg.ProcessGroupID == 0 {
		return renderToolMessageWithStyles(msg, width, styles)
	}
	inner := renderToolMessageWithStyles(msg, max(8, width-2), styles)
	if inner == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(inner, "\n"), "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func renderToolMessageWithStyles(msg ChatMessage, width int, styles transcriptStyles) string {
	label := msg.ToolName
	if label == "" {
		label = "tool"
	}
	var args map[string]interface{}
	_ = json.Unmarshal([]byte(msg.ToolArgs), &args)
	if args == nil {
		args = map[string]interface{}{}
	}

	done := !msg.IsRunning && (msg.Content != "" || msg.Duration > 0)
	action := toolAction(label, args, done)
	if msg.IsSkipped {
		action = replaceToolActionVerb(action, "Skipped")
	} else if msg.IsError {
		action = replaceToolActionVerb(action, "Failed")
	}
	isCmd := isCommandTool(label)
	var sb strings.Builder
	if !done {
		sb.WriteString(toolHeaderLineWithStyles(label, action, true, false, isCmd, 0, width, styles) + "\n")
		if detail := strings.TrimSpace(msg.RunningDetail); detail != "" {
			sb.WriteString("  " + glyphCorner + " " + truncateToWidth(detail, width-6) + "\n")
		}
		if isCmd {
			sb.WriteString(renderCommandOutputBlockWithStyles(label, msg.Content, width, styles))
		} else if result := toolResultLine(label, msg.Content, width-6); result != "" {
			sb.WriteString("  " + glyphCorner + " " + result + "\n")
		}
		return sb.String()
	}

	// Codex-style file-change rendering for patch: a "<Verb> <file> (+N -M)"
	// header plus a bounded, colored diff (line-number gutter for new files).
	// Falls back to the generic path when the V4A patch input isn't available
	// (e.g. legacy/test messages without ToolArgs).
	if label == "patch" && !msg.IsError {
		if patch, _ := args["patch"].(string); strings.TrimSpace(patch) != "" {
			if cell := renderPatchCellWithStyles(patch, msg.Duration, width, maxPatchPreviewLines, styles); cell != "" {
				return cell
			}
		}
	}
	// write_file returns a "Created/Edited <path> (+A -B)" header plus a bounded
	// unified diff (W2d); render it colored instead of an all-added dump.
	if label == "write_file" && !msg.IsError {
		if cell := renderWriteFileCellWithStyles(msg.Content, msg.Duration, width, styles); cell != "" {
			return cell
		}
	}
	// update_plan renders as a Codex-style checklist with progress, not a
	// one-line summary.
	if label == "update_plan" && !msg.IsError {
		if cell := renderPlanCellWithStyles(msg.Content, msg.Duration, width, styles); cell != "" {
			return cell
		}
	}

	// The red bullet conveys failure; duration stays visually subordinate.
	sb.WriteString(toolHeaderLineWithStyles(label, action, false, msg.IsError, isCmd && !msg.IsSkipped, msg.Duration, width, styles))
	sb.WriteString("\n")
	if !isCmd {
		if result := toolResultLine(label, msg.Content, width-6); result != "" {
			sb.WriteString("  " + glyphCorner + " " + result + "\n")
		}
	}
	// Command tools use a bounded physical-row head/tail preview. File-editing
	// tools retain their compact colored diff.
	if block := renderCommandOutputBlockWithStyles(label, msg.Content, width, styles); block != "" {
		sb.WriteString(block)
	} else if !msg.IsError {
		if diff := renderToolDiffWithStyles(label, args, width-4, styles); diff != "" {
			sb.WriteString(diff)
		}
	}
	return sb.String()
}

func replaceToolActionVerb(action, verb string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return verb + " tool"
	}
	_, rest, found := strings.Cut(action, " ")
	if !found || strings.TrimSpace(rest) == "" {
		return verb + " tool"
	}
	return verb + " " + rest
}

// renderCommandOutputBlock shows a bounded physical-row head/tail preview of
// raw command output (stdout/stderr) as dim lines, Codex-style. Keeping the tail
// preserves compiler and shell diagnostics without letting long output take
// over the transcript. It only applies to command tools.
func renderCommandOutputBlockWithStyles(label, content string, width int, styles transcriptStyles) string {
	switch label {
	case "terminal", "execute_command", "shell":
	default:
		return ""
	}
	if strings.TrimSpace(sanitizeTerminalText(content)) == "" {
		return ""
	}
	lines := boundedCommandOutputRows(content, max(8, width-4), maxCommandOutputRows)
	var sb strings.Builder
	for i, ln := range lines {
		prefix := "    "
		if i == 0 {
			prefix = "  " + glyphCorner + " "
		}
		sb.WriteString(prefix + styles.diffContext.Render(ln) + "\n")
	}
	return sb.String()
}

// renderToolDiff shows a compact, colored diff for file-editing tools so the
// user can see what actually changed (codex-style), instead of an opaque
// "Edited with patch". It bounds the output to a handful of lines.
func renderToolDiffWithStyles(label string, args map[string]interface{}, width int, styles transcriptStyles) string {
	const maxLines = 12
	var lines []string
	switch label {
	case "patch":
		patch, _ := args["patch"].(string)
		lines = strings.Split(strings.TrimRight(patch, "\n"), "\n")
	case "write_file":
		content, _ := args["content"].(string)
		if strings.TrimSpace(content) == "" {
			return ""
		}
		// A write is an all-added block; show it as added lines.
		for _, ln := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
			lines = append(lines, "+"+ln)
		}
	default:
		return ""
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	added, removed, shown := 0, 0, 0
	for _, ln := range lines {
		var styled string
		switch {
		case strings.HasPrefix(ln, "+"):
			added++
			styled = styles.diffAdd.Render(truncateToWidth(ln, width-4))
		case strings.HasPrefix(ln, "-"):
			removed++
			styled = styles.diffDel.Render(truncateToWidth(ln, width-4))
		default:
			styled = styles.diffContext.Render(truncateToWidth(ln, width-4))
		}
		if shown < maxLines {
			sb.WriteString("  " + glyphCorner + " " + styled + "\n")
			shown++
		}
	}
	if len(lines) > maxLines {
		sb.WriteString("  " + glyphCorner + " " + styles.diffContext.Render(fmt.Sprintf("… %d more line(s)", len(lines)-maxLines)) + "\n")
	}
	if added > 0 || removed > 0 {
		sb.WriteString("  " + glyphCorner + " " + styles.diffContext.Render(fmt.Sprintf("(+%d −%d)", added, removed)) + "\n")
	}
	return sb.String()
}

// maxPatchPreviewLines bounds the diff body shown inline at commit time. Real
// edits are short and show in full; large changes collapse with a remainder
// note. The full diff is available in the history browser overlay (H4).
const maxPatchPreviewLines = 40

type patchDiffLine struct {
	kind byte // '+', '-', ' ' (context), '@' (hunk hint)
	text string
}

type patchFileChange struct {
	verb  string // Added / Edited / Deleted / Moved
	path  string
	lines []patchDiffLine
}

// parseV4APatch parses the SelfMind/Codex V4A patch format into per-file
// changes. V4A carries no absolute line numbers (that is its design), so the
// renderer only numbers brand-new files, where lines are 1..N by construction.
func parseV4APatch(patch string) []patchFileChange {
	var files []patchFileChange
	var cur *patchFileChange
	push := func() {
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}
	start := func(verb, line, prefix string) {
		push()
		cur = &patchFileChange{verb: verb, path: strings.TrimSpace(strings.TrimPrefix(line, prefix))}
	}
	for _, ln := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(ln, "*** Begin Patch"), strings.HasPrefix(ln, "*** End Patch"):
			continue
		case strings.HasPrefix(ln, "*** Add File: "):
			start("Added", ln, "*** Add File: ")
		case strings.HasPrefix(ln, "*** Update File: "):
			start("Edited", ln, "*** Update File: ")
		case strings.HasPrefix(ln, "*** Delete File: "):
			start("Deleted", ln, "*** Delete File: ")
		case strings.HasPrefix(ln, "*** Move File: "):
			start("Moved", ln, "*** Move File: ")
		case strings.HasPrefix(ln, "@@"):
			if cur != nil {
				cur.lines = append(cur.lines, patchDiffLine{'@', strings.TrimSpace(strings.Trim(ln, "@ "))})
			}
		default:
			if cur == nil {
				continue
			}
			if ln == "" {
				cur.lines = append(cur.lines, patchDiffLine{' ', ""})
				continue
			}
			switch ln[0] {
			case '+':
				cur.lines = append(cur.lines, patchDiffLine{'+', ln[1:]})
			case '-':
				cur.lines = append(cur.lines, patchDiffLine{'-', ln[1:]})
			case ' ':
				cur.lines = append(cur.lines, patchDiffLine{' ', ln[1:]})
			default:
				cur.lines = append(cur.lines, patchDiffLine{' ', ln})
			}
		}
	}
	push()
	return files
}

// renderPatchCell renders a patch as Codex-style file-change cells: a header
// "<verb> <path> (+N -M)" and a colored, bounded diff body. maxLines bounds the
// total body lines (use a large value for the unbounded history view).
func renderPatchCellWithStyles(patch string, duration float64, width, maxLines int, styles transcriptStyles) string {
	files := parseV4APatch(patch)
	if len(files) == 0 {
		return ""
	}
	var sb strings.Builder
	budget := maxLines
	hidden := 0
	for fi, f := range files {
		added, removed := 0, 0
		for _, l := range f.lines {
			switch l.kind {
			case '+':
				added++
			case '-':
				removed++
			}
		}
		header := fmt.Sprintf("%s %s (%s %s)", styles.toolBulletDim.Render(glyphBullet), styleToolActionWithStyles("patch", f.verb+" "+f.path, styles),
			styles.diffAdd.Render(fmt.Sprintf("+%d", added)), styles.diffDel.Render(fmt.Sprintf("-%d", removed)))
		if fi == 0 && duration > 0 {
			header += fmt.Sprintf(" %.1fs", duration)
		}
		sb.WriteString(header + "\n")

		isAdd := f.verb == "Added"
		gutterW := len(fmt.Sprintf("%d", added))
		lineNo := 0
		for _, l := range f.lines {
			if l.kind == '+' && isAdd {
				lineNo++
			}
			if budget <= 0 {
				hidden++
				continue
			}
			switch l.kind {
			case '@':
				sb.WriteString("  " + styles.diffContext.Render("@@ "+l.text) + "\n")
			case '+':
				gutter := ""
				if isAdd {
					gutter = styles.diffContext.Render(fmt.Sprintf("%*d ", gutterW, lineNo))
				}
				sb.WriteString("  " + gutter + styles.diffAdd.Render("+ "+truncateToWidth(l.text, width-10)) + "\n")
			case '-':
				sb.WriteString("  " + styles.diffDel.Render("- "+truncateToWidth(l.text, width-10)) + "\n")
			default:
				sb.WriteString("  " + styles.diffContext.Render("  "+truncateToWidth(l.text, width-10)) + "\n")
			}
			budget--
		}
	}
	if hidden > 0 {
		sb.WriteString("  " + styles.diffContext.Render(fmt.Sprintf("… +%d more line(s) — open history to see the full diff", hidden)) + "\n")
	}
	return sb.String()
}

// maxWriteFileDiffPreview bounds the diff body shown inline for a write_file
// cell; the full diff is available via /search current.
const maxWriteFileDiffPreview = 30

// renderWriteFileCell renders write_file's "Created/Edited <path> (+A -B)"
// header plus its colored diff body. Legacy results (e.g. "Written N bytes")
// just render as the header line. Returns "" for empty content (fallback).
func renderWriteFileCellWithStyles(content string, duration float64, width int, styles transcriptStyles) string {
	content = strings.TrimRight(content, "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	var sb strings.Builder
	sb.WriteString(styles.toolBulletDim.Render(glyphBullet) + " " + styleToolActionWithStyles("write_file", lines[0], styles))
	if duration > 0 {
		sb.WriteString(fmt.Sprintf(" %.1fs", duration))
	}
	sb.WriteString("\n")
	shown := 0
	for _, ln := range lines[1:] {
		if shown >= maxWriteFileDiffPreview {
			break
		}
		var styled string
		switch {
		case strings.HasPrefix(ln, "+"):
			styled = styles.diffAdd.Render(truncateToWidth(ln, width-6))
		case strings.HasPrefix(ln, "-"):
			styled = styles.diffDel.Render(truncateToWidth(ln, width-6))
		default:
			styled = styles.diffContext.Render(truncateToWidth(ln, width-6))
		}
		sb.WriteString("  " + styled + "\n")
		shown++
	}
	if remaining := len(lines) - 1 - shown; remaining > 0 {
		sb.WriteString("  " + styles.diffContext.Render(fmt.Sprintf("… %d more line(s)", remaining)) + "\n")
	}
	return sb.String()
}

const (
	// maxPlanSteps is an extreme backstop only: a normal plan must render in full
	// so the user always perceives complete progress. Kept high enough that real
	// plans are never truncated; the "… N more steps" guard fires only for
	// pathological plans well beyond any legitimate size.
	maxPlanSteps  = 50
	glyphPlanDone = "✔" // ✔ completed
	glyphPlanBox  = "□" // □ pending / in-progress (codex distinguishes by color, not glyph)
)

// renderPlanCell renders update_plan as a Codex-style checklist (the "hybrid"
// look chosen for SelfMind): header `• Updated plan · done/total`, then a
// tree-indented block — an italic/dim explanation note, then one line per step
// marked ✔ (struck-through+dim) completed / □ (cyan+bold) in-progress / □ (dim)
// pending. Long notes and steps wrap to the terminal width with a hanging
// indent rather than being truncated. We keep the `· done/total` progress in the
// header (codex puts it in a persistent status bar, which SelfMind lacks).
// Returns "" only if content isn't parseable plan JSON (caller falls back).
func renderPlanCellWithStyles(content string, duration float64, width int, styles transcriptStyles) string {
	var payload struct {
		Explanation string `json:"explanation"`
		Plan        []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	if width < 20 {
		width = 20
	}
	completed := 0
	for _, s := range payload.Plan {
		if s.Status == "completed" {
			completed++
		}
	}

	// Build the indented content block (explanation note + steps) without the
	// tree prefix; the prefix is applied uniformly afterwards.
	var block []string
	if exp := strings.TrimSpace(payload.Explanation); exp != "" {
		for _, ln := range strings.Split(wrapText(exp, width-4), "\n") {
			block = append(block, styles.planExplain.Render(ln))
		}
	}
	if len(payload.Plan) == 0 {
		block = append(block, styles.planExplain.Render("(no steps provided)"))
	}
	shown := 0
	for _, s := range payload.Plan {
		if shown >= maxPlanSteps {
			break
		}
		block = append(block, planStepLinesWithStyles(strings.TrimSpace(s.Step), s.Status, width-4, styles)...)
		shown++
	}
	if len(payload.Plan) > maxPlanSteps {
		block = append(block, styles.planPending.Render(fmt.Sprintf("… %d more steps", len(payload.Plan)-maxPlanSteps)))
	}

	var sb strings.Builder
	sb.WriteString(styles.planSecondary.Render(glyphBullet) + " " + styles.planHeader.Render("Updated plan") +
		styles.planSecondary.Render(fmt.Sprintf(" · %d/%d", completed, len(payload.Plan))) + "\n")
	// Tree prefix: first block line gets "  └ ", the rest a flat 4-space indent.
	for i, ln := range block {
		if i == 0 {
			sb.WriteString(styles.planSecondary.Render("  └ ") + ln + "\n")
		} else {
			sb.WriteString("    " + ln + "\n")
		}
	}
	return sb.String()
}

// planStepLines renders one plan step into one or more styled lines: the status
// glyph + text on the first line, wrapped continuation lines hanging-indented
// under the text. The glyph is dimmed/colored by status; only completed step
// text is struck through (matching codex, which never strikes the glyph).
func planStepLinesWithStyles(text, status string, contentWidth int, styles transcriptStyles) []string {
	glyph := glyphPlanBox + " "
	glyphStyle := styles.planPending
	textStyle := styles.planPending
	switch status {
	case "completed":
		glyph = glyphPlanDone + " "
		glyphStyle = styles.planSecondary
		textStyle = styles.planDone
	case "in_progress":
		glyphStyle = styles.planActive
		textStyle = styles.planActive
	}
	stepWidth := contentWidth - 2 // account for the 2-col glyph / hanging indent
	if stepWidth < 4 {
		stepWidth = 4
	}
	wrapped := strings.Split(wrapText(text, stepWidth), "\n")
	out := make([]string, 0, len(wrapped))
	for i, ln := range wrapped {
		if i == 0 {
			out = append(out, glyphStyle.Render(glyph)+textStyle.Render(ln))
		} else {
			out = append(out, "  "+textStyle.Render(ln)) // hang under the glyph
		}
	}
	return out
}

// renderNoticeMessage renders a "notice" cell: exactly ONE compact line (long
// content is truncated), used as the durable transcript record for transient
// interactions such as approvals. The interactive detail lives in the active
// region, not in history.
func renderNoticeMessageWithTheme(content string, kind noticeKind, width int, t uitheme.Theme) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if width < 12 {
		width = 12
	}
	content = strings.ReplaceAll(content, "\n", " ")
	return noticeStyleWithTheme(kind, t).Render(truncateToWidth(content, width-1))
}

func renderSystemMessage(content string, width int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(glyphBullet + " Learning\n")
	if line := firstResultLine(content, width-6); line != "" {
		sb.WriteString("  " + glyphCorner + " " + line + "\n")
	}
	return sb.String()
}

func renderDigestMessageWithStyles(content string, width int, styles transcriptStyles) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if width < 12 {
		width = 12
	}
	lines := strings.Split(wrapText(content, width-4), "\n")
	var sb strings.Builder
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 {
			sb.WriteString(glyphBullet + " " + line + "\n")
			continue
		}
		sb.WriteString("  " + glyphCorner + " " + line + "\n")
	}
	return styles.toolEvidence.Render(strings.TrimRight(sb.String(), "\n"))
}

func toolAction(label string, args map[string]interface{}, done bool) string {
	detail := toolDetail(args, "path", "pattern", "query", "command", "name", "action")
	switch label {
	case "terminal", "execute_command", "shell":
		return commandToolAction(toolDetail(args, "command", "path"), done)
	case "cat", "read_file":
		detail = toolDetail(args, "path")
		if done {
			return "Read " + valueOr(detail, label)
		}
		return "Reading " + valueOr(detail, label)
	case "ls_r", "list_files":
		detail = toolDetail(args, "path")
		if done {
			return "Listed " + valueOr(detail, label)
		}
		return "Listing " + valueOr(detail, label)
	case "search_files", "grep":
		detail = toolDetail(args, "pattern", "query", "path")
		if done {
			return "Searched " + valueOr(detail, label)
		}
		return "Searching " + valueOr(detail, label)
	case "web_search":
		detail = toolDetail(args, "query")
		if done {
			return "Searched web for " + valueOr(detail, "query")
		}
		return "Searching web for " + valueOr(detail, "query")
	case "web_extract":
		detail = toolDetail(args, "url", "path")
		if done {
			return "Read " + valueOr(detail, "web page")
		}
		return "Reading " + valueOr(detail, "web page")
	case "batch_read":
		if done {
			return "Read file batch"
		}
		return "Reading file batch"
	case "patch":
		if done {
			return "Edited with patch"
		}
		return "Applying patch"
	case "write_file":
		if done {
			return "Wrote " + valueOr(detail, label)
		}
		return "Writing " + valueOr(detail, label)
	case "skill_manage":
		if done {
			return "Managed skill " + valueOr(detail, "")
		}
		return "Managing skill " + valueOr(detail, "")
	case "memory":
		if done {
			return "Updated memory"
		}
		return "Updating memory"
	case "session_search":
		if done {
			return "Searched sessions"
		}
		return "Searching sessions"
	case "update_plan":
		if done {
			return "Updated plan"
		}
		return "Updating plan"
	case "finish_run":
		if done {
			return "Finished run"
		}
		return "Finishing run"
	default:
		if done {
			return "Ran " + label
		}
		return "Running " + label
	}
}

func toolDetail(args map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(sanitizeTerminalText(v))
		}
	}
	return ""
}

func toolResultLine(label, content string, width int) string {
	switch label {
	case "ls_r", "list_files":
		return truncateToWidth(formatListFilesResult(content), width)
	case "search_files", "grep":
		return truncateToWidth(formatSearchFilesResult(content), width)
	case "update_plan":
		return truncateToWidth(formatPlanToolResult(content), width)
	case "finish_run":
		return truncateToWidth(formatFinishRunResult(content), width)
	case "patch":
		return truncateToWidth(formatPatchToolResult(content), width)
	case "terminal", "execute_command", "shell":
		return truncateToWidth(formatCommandResult(content), width)
	case "read_file", "cat":
		return truncateToWidth(formatFileReadResult(content), width)
	default:
		return truncateToWidth(formatGenericToolResult(content), width)
	}
}

// formatCommandResult condenses raw command output into a one-line header.
// renderCommandOutputBlock renders the head of the output separately, so this
// only needs to report shape: a line count, or the single line itself.
func formatCommandResult(content string) string {
	trimmed := strings.TrimRight(stripANSI(content), "\n")
	if strings.TrimSpace(trimmed) == "" {
		// Empty also covers the still-running state (output not captured yet);
		// the "Ran …" header already conveys a successful no-output command.
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 1 {
		return strings.TrimSpace(lines[0])
	}
	return fmt.Sprintf("%d lines", len(lines))
}

// formatFileReadResult reports the size of a file read instead of echoing its
// first content line, which is rarely meaningful on its own.
func formatFileReadResult(content string) string {
	if strings.TrimSpace(content) == "" {
		// Empty also covers the still-running state; show nothing rather than a
		// misleading "empty" while the read is in flight.
		return ""
	}
	lines := strings.Count(content, "\n") + 1
	return fmt.Sprintf("%d lines%s%s", lines, glyphDot, humanizeBytes(len(content)))
}

func humanizeBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatListFilesResult(content string) string {
	var payload struct {
		Count       int  `json:"count"`
		Scanned     int  `json:"scanned"`
		Truncated   bool `json:"truncated"`
		SkippedDirs int  `json:"skipped_dirs"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return firstResultLine(content, 80)
	}
	parts := []string{fmt.Sprintf("%d entries", payload.Count)}
	if payload.Scanned > payload.Count {
		parts = append(parts, fmt.Sprintf("%d scanned", payload.Scanned))
	}
	if payload.SkippedDirs > 0 {
		parts = append(parts, fmt.Sprintf("%d dirs skipped", payload.SkippedDirs))
	}
	if payload.Truncated {
		parts = append(parts, "truncated")
	}
	return strings.Join(parts, glyphDot)
}

func formatSearchFilesResult(content string) string {
	var payload struct {
		Count        int  `json:"count"`
		ScannedFiles int  `json:"scanned_files"`
		Truncated    bool `json:"truncated"`
		SkippedDirs  int  `json:"skipped_dirs"`
		SkippedLarge int  `json:"skipped_large"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return firstResultLine(content, 80)
	}
	parts := []string{fmt.Sprintf("%d matches", payload.Count)}
	if payload.ScannedFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d files scanned", payload.ScannedFiles))
	}
	if payload.SkippedDirs > 0 {
		parts = append(parts, fmt.Sprintf("%d dirs skipped", payload.SkippedDirs))
	}
	if payload.SkippedLarge > 0 {
		parts = append(parts, fmt.Sprintf("%d large files skipped", payload.SkippedLarge))
	}
	if payload.Truncated {
		parts = append(parts, "truncated")
	}
	return strings.Join(parts, glyphDot)
}

func formatPlanToolResult(content string) string {
	var payload struct {
		Explanation string `json:"explanation"`
		Plan        []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil || len(payload.Plan) == 0 {
		return "plan updated"
	}
	inProgress := ""
	completed := 0
	for _, step := range payload.Plan {
		switch step.Status {
		case "in_progress":
			inProgress = strings.TrimSpace(step.Step)
		case "completed":
			completed++
		}
	}
	if inProgress != "" {
		return fmt.Sprintf("%d steps%snow: %s", len(payload.Plan), glyphDot, inProgress)
	}
	return fmt.Sprintf("%d steps%s%d completed", len(payload.Plan), glyphDot, completed)
}

func formatFinishRunResult(content string) string {
	var payload struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	status := strings.TrimSpace(payload.Status)
	summary := strings.TrimSpace(payload.Summary)
	switch {
	case status != "" && summary != "":
		return status + glyphDot + summary
	case summary != "":
		return summary
	case status != "":
		return status
	default:
		return ""
	}
}

func formatPatchToolResult(content string) string {
	var payload struct {
		Success       bool     `json:"Success"`
		FilesModified []string `json:"FilesModified"`
		FilesCreated  []string `json:"FilesCreated"`
		FilesDeleted  []string `json:"FilesDeleted"`
		Error         string   `json:"Error"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return firstResultLine(content, 80)
	}
	if strings.TrimSpace(payload.Error) != "" && !payload.Success {
		return firstResultLine(payload.Error, 80)
	}
	parts := make([]string, 0, 3)
	if len(payload.FilesModified) > 0 {
		parts = append(parts, fmt.Sprintf("modified %s", summarizeToolPaths(payload.FilesModified)))
	}
	if len(payload.FilesCreated) > 0 {
		parts = append(parts, fmt.Sprintf("created %s", summarizeToolPaths(payload.FilesCreated)))
	}
	if len(payload.FilesDeleted) > 0 {
		parts = append(parts, fmt.Sprintf("deleted %s", summarizeToolPaths(payload.FilesDeleted)))
	}
	if len(parts) == 0 {
		if payload.Success {
			return "patch applied"
		}
		return ""
	}
	return strings.Join(parts, glyphDot)
}

func formatGenericToolResult(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(content), &obj); err == nil && len(obj) > 0 {
		for _, key := range []string{"message", "summary", "status", "error", "Error"} {
			if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		for _, key := range []string{"FilesModified", "files_modified", "modified", "files"} {
			if paths := interfaceStringSlice(obj[key]); len(paths) > 0 {
				return "modified " + summarizeToolPaths(paths)
			}
		}
		for _, key := range []string{"FilesCreated", "files_created", "created"} {
			if paths := interfaceStringSlice(obj[key]); len(paths) > 0 {
				return "created " + summarizeToolPaths(paths)
			}
		}
		for _, key := range []string{"FilesDeleted", "files_deleted", "deleted"} {
			if paths := interfaceStringSlice(obj[key]); len(paths) > 0 {
				return "deleted " + summarizeToolPaths(paths)
			}
		}
		if value, ok := obj["Success"].(bool); ok && value {
			return "completed"
		}
		if value, ok := obj["success"].(bool); ok && value {
			return "completed"
		}
		return fmt.Sprintf("%d fields", len(obj))
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(content), &arr); err == nil {
		return fmt.Sprintf("%d items", len(arr))
	}
	return firstResultLine(content, 80)
}

func interfaceStringSlice(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	default:
		return nil
	}
}

func summarizeToolPaths(paths []string) string {
	cleaned := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			cleaned = append(cleaned, path)
		}
	}
	switch len(cleaned) {
	case 0:
		return "0 files"
	case 1:
		return cleaned[0]
	case 2:
		return cleaned[0] + ", " + cleaned[1]
	default:
		return fmt.Sprintf("%s, %s +%d more", cleaned[0], cleaned[1], len(cleaned)-2)
	}
}

func firstResultLine(content string, width int) string {
	content = stripANSI(content)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return truncateToWidth(line, width)
		}
	}
	return ""
}
