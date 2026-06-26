package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const (
	glyphBullet  = "\u2022"
	glyphCorner  = "\u2514\u2500"
	glyphChevron = "\u203a"
	glyphDot     = " · "
)

func (m *uiModel) renderAllMessages() string {
	st := m.common.Styles
	w := m.viewport.Width
	if w <= 0 {
		w = 60
	}

	var allLines []string
	processLines := func(lines []string, baseIdx int) []string {
		if !m.mouseSelection {
			return lines
		}
		start, end := m.mouseSelectionRange()
		out := append([]string{}, lines...)
		for i, line := range out {
			idx := baseIdx + i
			if idx < start || idx > end {
				continue
			}
			out[i] = renderSelectedTranscriptLine(line, w, st.Chat.Selected)
		}
		return out
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

	if strings.TrimSpace(m.liveStreamContent) != "" {
		rendered := renderAssistantMessage(stripANSI(m.liveStreamContent), w)
		msgLines := strings.Split(rendered, "\n")
		msgLines = processLines(msgLines, len(allLines))
		allLines = append(allLines, msgLines...)
	}

	if m.thinking {
		allLines = append(allLines, "")
		spinnerView := m.spinner.View()
		dots := strings.Repeat(".", (m.thinkingDots%3)+1)
		label := strings.TrimSpace(m.activityText)
		if label == "" {
			label = "Working"
		}
		rendered := st.Chat.Thinking.Render(spinnerView + " " + label + dots)
		lines := processLines([]string{rendered}, len(allLines))
		allLines = append(allLines, lines...)
		allLines = append(allLines, "")
	}

	minLines := m.viewport.Height + m.viewport.YOffset
	for len(allLines) < minLines {
		idx := len(allLines)
		line := processLines([]string{""}, idx)
		allLines = append(allLines, line[0])
	}
	return strings.Join(allLines, "\n")
}

func renderSelectedTranscriptLine(line string, width int, style lipgloss.Style) string {
	if width < 1 {
		width = 1
	}
	return style.Copy().Width(width).Render(truncateToWidth(stripANSI(line), width))
}

func (m *uiModel) renderStartupCard(width int) []string {
	maxCardW := width - 2
	if maxCardW > 54 {
		maxCardW = 54
	}
	if maxCardW < 24 {
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
	title := ">_ SelfMind (v0.1.0)"
	modelLine := "model:     " + modelName + "      /model to change"
	providerLine := ""
	if providerName != "" && providerName != modelName && providerName != "active" {
		providerLine = "provider:  " + providerName
	}
	dirLine := "directory: " + currentWorkingDir()

	needed := runewidth.StringWidth(title)
	for _, line := range []string{modelLine, providerLine, dirLine} {
		if width := runewidth.StringWidth(line); width > needed {
			needed = width
		}
	}
	cardW := needed + 4
	if cardW < 48 {
		cardW = 48
	}
	if cardW > maxCardW {
		cardW = maxCardW
	}

	lines := []string{
		"+" + strings.Repeat("-", cardW-2) + "+",
		renderBoxLine(title, cardW),
		renderBoxLine("", cardW),
		renderBoxLine(modelLine, cardW),
	}
	if providerLine != "" {
		lines = append(lines, renderBoxLine(providerLine, cardW))
	}
	lines = append(lines,
		renderBoxLine(dirLine, cardW),
		"+"+strings.Repeat("-", cardW-2)+"+",
		"",
		"Tip: Tell SelfMind what to inspect, change, test, or remember.",
		"",
	)
	return lines
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
		return "\n" + style.Render(glyphChevron+" ")
	}
	wrapped := wrapText(content, width-4)
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = glyphChevron + " " + line
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
		lines[i] = "  " + line
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

	done := !msg.IsRunning && (msg.Content != "" || msg.Duration > 0)
	action := toolAction(label, args, done)
	var sb strings.Builder
	if !done {
		sb.WriteString(glyphBullet + " " + action + "\n")
		if detail := strings.TrimSpace(msg.RunningDetail); detail != "" {
			sb.WriteString("  " + glyphCorner + " " + truncateToWidth(detail, width-6) + "\n")
		}
		if result := toolResultLine(label, msg.Content, width-6); result != "" {
			sb.WriteString("  " + glyphCorner + " " + result + "\n")
		}
		return sb.String()
	}

	status := ""
	if msg.IsError {
		status = " failed"
	}
	sb.WriteString(fmt.Sprintf("%s %s%s", glyphBullet, action, status))
	if msg.Duration > 0 {
		sb.WriteString(fmt.Sprintf(" %.1fs", msg.Duration))
	}
	sb.WriteString("\n")
	if result := toolResultLine(label, msg.Content, width-6); result != "" {
		sb.WriteString("  " + glyphCorner + " " + result + "\n")
	}
	return sb.String()
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

func toolAction(label string, args map[string]interface{}, done bool) string {
	detail := toolDetail(args, "path", "pattern", "query", "command", "name", "action")
	switch label {
	case "terminal", "execute_command", "shell":
		detail = toolDetail(args, "command", "path")
		if done {
			return "Ran " + valueOr(detail, label)
		}
		return "Running " + valueOr(detail, label)
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
			return strings.TrimSpace(v)
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
	default:
		return truncateToWidth(formatGenericToolResult(content), width)
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

func renderBoxLine(content string, width int) string {
	inner := width - 4
	if inner < 1 {
		return content
	}
	text := truncateToWidth(content, inner)
	return "| " + padRightWidth(text, inner) + " |"
}

func padRightWidth(s string, width int) string {
	pad := width - runewidth.StringWidth(stripANSI(s))
	if pad < 0 {
		pad = 0
	}
	return s + strings.Repeat(" ", pad)
}
