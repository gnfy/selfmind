package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

func (m *uiModel) renderAllMessages() string {
	st := m.common.Styles
	w := m.viewport.Width
	if w <= 0 {
		w = 60
	}

	var allLines []string

	// Calculate selection range.
	startY, endY := m.selectionStart, m.selectionEnd
	if startY > endY {
		startY, endY = endY, startY
	}
	viewportTop := 0
	scrollOffset := m.viewport.YOffset
	lineStart := scrollOffset + (startY - viewportTop)
	lineEnd := scrollOffset + (endY - viewportTop)

	processLines := func(lines []string, baseIdx int) []string {
		if !m.isSelecting {
			return lines
		}
		for i := range lines {
			globalLineIdx := baseIdx + i
			if globalLineIdx >= lineStart && globalLineIdx <= lineEnd {
				plain := stripANSI(lines[i])
				if strings.TrimSpace(plain) == "" {
					continue
				}
				lines[i] = st.Chat.Selected.Render(plain)
			}
		}
		return lines
	}

	startupLines := append([]string{"", ""}, m.renderStartupCard(w)...)
	startupLines = processLines(startupLines, 0)
	allLines = append(allLines, startupLines...)

	for _, msg := range m.messages {
		var rendered string
		switch msg.Role {
		case "user":
			rendered = renderUserMessage(stripANSI(msg.Content), w)
		case "assistant":
			rendered = renderAssistantMessage(stripANSI(msg.Content), w)
		case "tool":
			rendered = renderToolMessage(msg, w)
		case "system":
			rendered = renderSystemMessage(stripANSI(msg.Content), w)
		}

		msgLines := strings.Split(rendered, "\n")
		msgLines = processLines(msgLines, len(allLines))
		allLines = append(allLines, msgLines...)
	}

	if m.thinking {
		spinnerView := m.spinner.View()
		dots := strings.Repeat(".", (m.thinkingDots%3)+1)
		rendered := st.Chat.Thinking.Render(spinnerView + " Working" + dots)
		lines := processLines([]string{rendered}, len(allLines))
		allLines = append(allLines, lines...)
	}

	minLines := m.viewport.Height + m.viewport.YOffset
	for len(allLines) < minLines {
		allLines = append(allLines, "")
	}

	return strings.Join(allLines, "\n")
}

func (m *uiModel) renderStartupCard(width int) []string {
	maxCardW := width - 2
	if maxCardW > 54 {
		maxCardW = 54
	}
	if maxCardW < 24 {
		return []string{m.common.Styles.Welcome}
	}

	modelName := m.modelName
	if modelName == "" {
		modelName = m.providerName
	}
	if modelName == "" {
		modelName = "active"
	}
	title := ">_ SelfMind (v0.1.0)"
	modelLine := "model:     " + modelName + "      /model to change"
	dirLine := "directory: " + currentWorkingDir()

	needed := runewidth.StringWidth(title)
	for _, line := range []string{modelLine, dirLine} {
		if w := runewidth.StringWidth(line); w > needed {
			needed = w
		}
	}

	cardW := needed + 4
	if cardW < 48 {
		cardW = 48
	}
	if cardW > maxCardW {
		cardW = maxCardW
	}

	return []string{
		"┌" + strings.Repeat("─", cardW-2) + "┐",
		renderBoxLine(title, cardW),
		renderBoxLine("", cardW),
		renderBoxLine(modelLine, cardW),
		renderBoxLine(dirLine, cardW),
		"└" + strings.Repeat("─", cardW-2) + "┘",
		"",
		"Tip: Tell SelfMind what to inspect, change, test, or remember.",
		"",
	}
}

func renderUserMessage(content string, width int) string {
	content = strings.TrimRight(content, "\n")
	if width < 8 {
		width = 8
	}
	style := lipgloss.NewStyle().
		Background(lipgloss.Color("236")).
		Foreground(lipgloss.Color("255")).
		Padding(1, 1).
		Width(width)
	if content == "" {
		return "\n" + style.Render("›")
	}
	wrapped := wrapText(content, width-4)
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = "› " + line
		} else {
			lines[i] = "  " + line
		}
	}
	return "\n" + style.Render(strings.Join(lines, "\n"))
}

func renderAssistantMessage(content string, width int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	body := strings.TrimRight(renderMarkdown(content, width-4), "\n")
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		if i == 0 {
			lines[i] = "• " + line
		} else {
			lines[i] = "  " + line
		}
	}
	return "\n" + strings.Join(lines, "\n")
}

func renderToolMessage(msg ChatMessage, width int) string {
	label := msg.ToolName
	if label == "" {
		label = "tool"
	}

	var args map[string]interface{}
	_ = json.Unmarshal([]byte(msg.ToolArgs), &args)
	if args == nil {
		args = map[string]interface{}{}
	}

	done := msg.Content != ""
	action := toolAction(label, args, done)
	if !done {
		return "• " + action + "\n"
	}

	dur := fmt.Sprintf("%.1fs", msg.Duration)
	if msg.Duration == 0 {
		dur = "0.1s"
	}

	status := ""
	if msg.IsError {
		status = " failed"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("• %s%s  %s\n", action, status, dur))
	if result := firstResultLine(msg.Content, width-6); result != "" {
		sb.WriteString("  └─ " + result + "\n")
	}
	return sb.String()
}

func renderSystemMessage(content string, width int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("• Learning\n")
	if line := firstResultLine(content, width-6); line != "" {
		sb.WriteString("  └─ " + line + "\n")
	}
	return sb.String()
}

func toolAction(label string, args map[string]interface{}, done bool) string {
	detail := toolDetail(args, "path", "pattern", "query", "command", "name", "action")
	switch label {
	case "terminal", "execute_command", "shell":
		if done {
			return "Ran " + valueOr(detail, label)
		}
		return "Running " + valueOr(detail, label)
	case "cat", "read_file":
		if done {
			return "Read " + valueOr(detail, label)
		}
		return "Reading " + valueOr(detail, label)
	case "ls_r", "list_files", "search_files", "grep":
		if done {
			return "Searched " + valueOr(detail, label)
		}
		return "Searching " + valueOr(detail, label)
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
			return strings.TrimSpace(v)
		}
	}
	return ""
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

func renderBoxLine(content string, width int) string {
	inner := width - 4
	if inner < 1 {
		return content
	}
	text := truncateToWidth(content, inner)
	return "│ " + padRightWidth(text, inner) + " │"
}

func padRightWidth(s string, width int) string {
	pad := width - runewidth.StringWidth(stripANSI(s))
	if pad < 0 {
		pad = 0
	}
	return s + strings.Repeat(" ", pad)
}
