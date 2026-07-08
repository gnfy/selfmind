package cli

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"

	uicommon "selfmind/internal/ui/common"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// cellRenderer turns one ChatMessage into its rendered (pre-split) string for a
// given width. The registry lets new transcript cell kinds (e.g. approval
// cards, plan widgets) plug in without touching renderAllMessages — the
// extensibility hook for later phases.
type cellRenderer func(msg ChatMessage, width int) string

var cellRenderers = map[string]cellRenderer{
	"user":      func(m ChatMessage, w int) string { return renderUserMessage(stripANSI(m.Content), w) },
	"assistant": func(m ChatMessage, w int) string { return renderAssistantMessage(stripANSI(m.Content), w) },
	"tool":      func(m ChatMessage, w int) string { return renderToolMessage(m, w) },
	"system":    func(m ChatMessage, w int) string { return renderSystemMessage(stripANSI(m.Content), w) },
	"notice":    func(m ChatMessage, w int) string { return renderNoticeMessage(stripANSI(m.Content), w) },
}

// renderCell dispatches a message to its registered renderer. Unknown roles
// render to empty (matching the previous switch default).
func renderCell(msg ChatMessage, width int) string {
	if r, ok := cellRenderers[msg.Role]; ok {
		return r(msg, width)
	}
	return ""
}

// renderCache memoizes per-message rendered output keyed by a fingerprint of the
// message's render-relevant fields plus width. Finalized messages have a stable
// fingerprint, so their expensive markdown/tool rendering runs once and is
// reused across the many frames bubbletea draws on cosmetic ticks (spinner,
// cursor blink, status, stream flush). This turns the per-frame transcript cost
// from "re-render all history" into "re-fingerprint all history" — roughly a
// thousandfold cheaper. A bounded live window (later phase) removes the
// remaining linear scan.
type renderCache struct {
	width   int
	entries map[uint64]string
}

// maxRenderCacheEntries caps memory: transient states (a running tool's
// heartbeat updates, streaming) each mint an entry that becomes garbage once
// finalized. When the map grows past this, drop it whole; stable messages
// simply repopulate on the next frame.
const maxRenderCacheEntries = 4096

func (c *renderCache) lookup(fp uint64) (string, bool) {
	if c == nil || c.entries == nil {
		return "", false
	}
	s, ok := c.entries[fp]
	return s, ok
}

func (c *renderCache) store(fp uint64, rendered string) {
	if c.entries == nil || len(c.entries) > maxRenderCacheEntries {
		c.entries = make(map[uint64]string, 64)
	}
	c.entries[fp] = rendered
}

// resetForWidth clears the cache when the wrap width changes; cached renders are
// width-specific and would be wrong after a resize.
func (c *renderCache) resetForWidth(width int) {
	c.width = width
	c.entries = make(map[uint64]string, 64)
}

// messageFingerprint is a cheap content+state hash. Any change to a field that
// affects rendering (content, tool metadata, running/error state, duration,
// width) produces a new fingerprint and so a cache miss. Hashing the full
// content each frame is still O(content), but ~1000x cheaper than re-running
// markdown/tool rendering over it.
func messageFingerprint(msg ChatMessage, width int) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(uint32(width)))
	_, _ = h.Write(buf[:])
	writeField := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	writeField(msg.Role)
	writeField(msg.Content)
	writeField(msg.ToolName)
	writeField(msg.ToolArgs)
	writeField(msg.ToolCallID)
	writeField(msg.RunningDetail)
	var flags byte
	if msg.IsRunning {
		flags |= 1
	}
	if msg.IsError {
		flags |= 2
	}
	_, _ = h.Write([]byte{flags})
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(msg.Duration))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

const (
	glyphBullet  = "\u2022"
	glyphCorner  = "\u2514" // \u2514 tree connector (codex-style; used as `glyphCorner + " "`)
	glyphChevron = "\u203a"
	// Notification glyphs (see notificationStyleFor). Kept to widely-supported
	// code points so they render across terminals without width surprises.
	glyphCheck     = "\u2713" // checkmark: success
	glyphArrowInto = "\u21b3" // down-right arrow: steering injected into the run
	glyphWarning   = "\u26a0" // warning sign: recoverable problem
	glyphCross     = "\u2717" // ballot X: cancelled/aborted
	glyphDot       = " · "
)

func (m *uiModel) renderAllMessages() string {
	st := m.common.Styles
	w := m.viewport.Width
	if w <= 0 {
		w = 60
	}

	if m.transcriptCache == nil {
		m.transcriptCache = &renderCache{}
	}
	if m.transcriptCache.width != w {
		m.transcriptCache.resetForWidth(w)
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

	for i := 0; i < len(m.messages); {
		// Group a run of consecutive read-only tool cells into one codex-style
		// "Explored" cell, rather than one cell per read.
		if isExploreCell(m.messages[i]) {
			j := i + 1
			for j < len(m.messages) && isExploreCell(m.messages[j]) {
				j++
			}
			group := m.messages[i:j]
			fp := exploreGroupFingerprint(group, w)
			rendered, ok := m.transcriptCache.lookup(fp)
			if !ok {
				rendered = renderExploreGroup(group, w)
				m.transcriptCache.store(fp, rendered)
			}
			msgLines := processLines(strings.Split(rendered, "\n"), len(allLines))
			allLines = append(allLines, msgLines...)
			i = j
			continue
		}
		msg := m.messages[i]
		fp := messageFingerprint(msg, w)
		rendered, ok := m.transcriptCache.lookup(fp)
		if !ok {
			rendered = renderCell(msg, w)
			m.transcriptCache.store(fp, rendered)
		}
		msgLines := processLines(strings.Split(rendered, "\n"), len(allLines))
		allLines = append(allLines, msgLines...)
		i++
	}

	if strings.TrimSpace(m.liveStreamContent) != "" {
		rendered := renderAssistantMessage(stripANSI(m.liveStreamContent), w)
		msgLines := strings.Split(rendered, "\n")
		msgLines = processLines(msgLines, len(allLines))
		allLines = append(allLines, msgLines...)
	}

	// Suppressed while the approval panel is up (mirrors renderActiveBlock):
	// "Preparing to run <tool>…" next to the panel is duplicated noise.
	if m.thinking && m.approvalPrompt == nil {
		allLines = append(allLines, "")
		spinnerView := m.spinner.View()
		dots := strings.Repeat(".", (m.thinkingDots%3)+1)
		label := strings.TrimSpace(m.activityText)
		if label == "" {
			label = "Working"
		}
		// Codex-style suffix: dim "(elapsed · esc to interrupt)" so a long run
		// shows progress and how to stop it.
		hint := ""
		if !m.thinkingStart.IsZero() {
			hint = toolBulletDim.Render(fmt.Sprintf("  (%s · esc to interrupt)", formatElapsedCompact(time.Since(m.thinkingStart))))
		}
		rendered := st.Chat.Thinking.Render(spinnerView+" "+label+dots) + hint
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

var (
	startupBorderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteBorder))
	startupLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteMuted))
	startupValueStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteText))
	startupSubtleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteSubtle))
	startupCommandStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteBlue))
)

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
		startupBorderStyle.Render("+" + strings.Repeat("-", cardW-2) + "+"),
		renderStartupBoxLine(startupValueStyle.Render(">_ SelfMind ")+startupSubtleStyle.Render("(v0.1.0)"), cardW),
		renderStartupBoxLine("", cardW),
		renderStartupDataLine("model:", modelName, cardW, "      /model to change"),
	}
	if providerLine != "" {
		lines = append(lines, renderStartupDataLine("provider:", providerName, cardW, ""))
	}
	lines = append(lines,
		renderStartupDataLine("directory:", currentWorkingDir(), cardW, ""),
		startupBorderStyle.Render("+"+strings.Repeat("-", cardW-2)+"+"),
		"",
		startupValueStyle.Render("Tip: Tell SelfMind what to inspect, change, test, or remember."),
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
		Background(lipgloss.Color(uicommon.PaletteEditorBG)).
		Foreground(lipgloss.Color(uicommon.PaletteEditorText)).
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

// currentMarkerStyle highlights the gateway's "← current" marker (e.g. in the
// /workspaces list) so the active selection stands out — same cyan family as
// the command hints. Applied at render time only; the gateway text stays plain
// for IM surfaces.
func renderStartupBoxLine(content string, width int) string {
	inner := width - 4
	if inner < 1 {
		return content
	}
	return startupBorderStyle.Render("| ") + padRightWidth(content, inner) + startupBorderStyle.Render(" |")
}

func renderStartupDataLine(label, value string, width int, suffix string) string {
	const labelWidth = 11
	inner := width - 4
	if inner < 1 {
		return ""
	}
	labelText := label + strings.Repeat(" ", max(0, labelWidth-runewidth.StringWidth(label)))
	suffixWidth := runewidth.StringWidth(suffix)
	valueWidth := inner - labelWidth - suffixWidth
	if valueWidth < 1 {
		valueWidth = inner - labelWidth
		suffix = ""
	}
	if valueWidth < 1 {
		return renderStartupBoxLine(startupLabelStyle.Render(truncateToWidth(labelText, inner)), width)
	}

	valueText := truncateToWidth(value, valueWidth)
	renderedSuffix := ""
	if suffix != "" {
		renderedSuffix = strings.Replace(suffix, "/model", startupCommandStyle.Render("/model"), 1)
		renderedSuffix = strings.Replace(renderedSuffix, "to change", startupSubtleStyle.Render("to change"), 1)
	}
	content := startupLabelStyle.Render(labelText) + startupValueStyle.Render(valueText) + renderedSuffix
	if runewidth.StringWidth(stripANSI(content)) > inner {
		content = startupLabelStyle.Render(labelText) + startupValueStyle.Render(truncateToWidth(value, inner-labelWidth))
	}
	return renderStartupBoxLine(content, width)
}

var currentMarkerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(uicommon.PaletteBlue))

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
		if strings.Contains(line, "← current") {
			line = strings.ReplaceAll(line, "← current", currentMarkerStyle.Render("← current"))
		}
		lines[i] = "  " + line
	}
	return "\n" + strings.Join(lines, "\n")
}

var (
	toolHeaderStyle = lipgloss.NewStyle().Bold(true)                      // bold action title (codex: "Explored"/"Ran")
	toolBulletRun   = lipgloss.NewStyle().Faint(true)                     // ◦ dim while running
	toolBulletOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // • green: command succeeded
	toolBulletErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // • red: failed
	toolBulletDim   = lipgloss.NewStyle().Faint(true)                     // • dim: non-command done
)

const glyphBulletHollow = "◦"

func isCommandTool(label string) bool {
	switch label {
	case "terminal", "execute_command", "shell":
		return true
	}
	return false
}

// toolHeaderLine renders the codex-style cell header: a status bullet (◦ dim
// while running, • green on command success, • red on failure, • dim otherwise)
// followed by the bold action title.
func toolHeaderLine(action string, running, isErr, isCommand bool) string {
	var bullet string
	switch {
	case running:
		bullet = toolBulletRun.Render(glyphBulletHollow)
	case isErr:
		bullet = toolBulletErr.Render(glyphBullet)
	case isCommand:
		bullet = toolBulletOK.Render(glyphBullet)
	default:
		bullet = toolBulletDim.Render(glyphBullet)
	}
	return bullet + " " + toolHeaderStyle.Render(action)
}

// exploreVerbStyle colors the Read/List/Search verb in an "Explored" group
// (codex renders these cyan; 39 is SelfMind's existing accent blue).
var exploreVerbStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))

// isExploreToolName reports whether a tool is a read-only "exploration" (file
// read, directory list, content search) that codex groups under one "Explored"
// cell. Mutating/command tools are rendered as their own cells.
func isExploreToolName(label string) bool {
	switch label {
	case "read_file", "cat", "list_files", "ls_r", "search_files", "grep":
		return true
	}
	return false
}

func isExploreCell(msg ChatMessage) bool {
	return msg.Role == "tool" && isExploreToolName(msg.ToolName)
}

// exploreEntry maps a read-only tool call to a (verb, argument) pair for the
// grouped Explored view: "Read <path>", "List <path>", "Search <q> in <path>".
func exploreEntry(label string, args map[string]interface{}) (verb, arg string, ok bool) {
	switch label {
	case "read_file", "cat":
		return "Read", toolDetail(args, "path"), true
	case "list_files", "ls_r":
		return "List", valueOr(toolDetail(args, "path"), "."), true
	case "search_files", "grep":
		q := toolDetail(args, "pattern", "query")
		p := toolDetail(args, "path")
		switch {
		case q != "" && p != "":
			return "Search", q + " in " + p, true
		case q != "":
			return "Search", q, true
		default:
			return "Search", p, true
		}
	}
	return "", "", false
}

// exploreLine renders one "<cyan verb> <arg>" line, wrapping a long argument
// with a hanging indent aligned under the argument (the verb is ASCII so its
// display width equals its byte length).
func exploreLine(verb, arg string, contentWidth int) []string {
	if contentWidth < 8 {
		contentWidth = 8
	}
	hang := len(verb) + 1
	avail := contentWidth - hang
	if avail < 4 {
		avail = 4
	}
	wrapped := strings.Split(wrapText(strings.TrimSpace(arg), avail), "\n")
	out := make([]string, 0, len(wrapped))
	for i, ln := range wrapped {
		if i == 0 {
			out = append(out, exploreVerbStyle.Render(verb)+" "+ln)
		} else {
			out = append(out, strings.Repeat(" ", hang)+ln)
		}
	}
	return out
}

// renderExploreGroup renders a run of consecutive read-only tool cells as a
// single codex-style "Explored" cell: a bold header (Exploring while any member
// is still running) and tree-indented verb lines. Consecutive reads collapse
// into one comma-joined "Read a, b, c" line; mixed verbs get a line each.
func renderExploreGroup(msgs []ChatMessage, width int) string {
	if width < 20 {
		width = 20
	}
	running := false
	type entry struct{ verb, arg string }
	var entries []entry
	allReads := true
	for _, m := range msgs {
		if m.IsRunning {
			running = true
		}
		var args map[string]interface{}
		_ = json.Unmarshal([]byte(m.ToolArgs), &args)
		verb, arg, ok := exploreEntry(m.ToolName, args)
		if !ok {
			continue
		}
		if verb != "Read" {
			allReads = false
		}
		entries = append(entries, entry{verb, arg})
	}
	if len(entries) == 0 {
		return ""
	}

	header := "Explored"
	bullet := toolBulletDim.Render(glyphBullet)
	if running {
		header = "Exploring"
		bullet = toolBulletRun.Render(glyphBulletHollow)
	}

	var block []string
	if allReads {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.arg != "" {
				names = append(names, e.arg)
			}
		}
		block = exploreLine("Read", strings.Join(names, ", "), width-4)
	} else {
		for _, e := range entries {
			block = append(block, exploreLine(e.verb, e.arg, width-4)...)
		}
	}

	var sb strings.Builder
	sb.WriteString(bullet + " " + toolHeaderStyle.Render(header) + "\n")
	for i, ln := range block {
		if i == 0 {
			sb.WriteString(planFaintStyle.Render("  └ ") + ln + "\n")
		} else {
			sb.WriteString("    " + ln + "\n")
		}
	}
	return sb.String()
}

// exploreGroupFingerprint combines the members' fingerprints (plus a salt so it
// never aliases a single-message entry) so a grouped Explored cell is cached
// like any other rendered cell.
func exploreGroupFingerprint(msgs []ChatMessage, width int) uint64 {
	h := uint64(1469598103934665603) ^ uint64(0x6578706c6f7265) // fnv offset ^ "explore"
	for _, m := range msgs {
		h = (h * 1099511628211) ^ messageFingerprint(m, width)
	}
	return h
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
	isCmd := isCommandTool(label)
	var sb strings.Builder
	if !done {
		sb.WriteString(toolHeaderLine(action, true, false, isCmd) + "\n")
		if detail := strings.TrimSpace(msg.RunningDetail); detail != "" {
			sb.WriteString("  " + glyphCorner + " " + truncateToWidth(detail, width-6) + "\n")
		}
		if result := toolResultLine(label, msg.Content, width-6); result != "" {
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
			if cell := renderPatchCell(patch, msg.Duration, width, maxPatchPreviewLines); cell != "" {
				return cell
			}
		}
	}
	// write_file returns a "Created/Edited <path> (+A -B)" header plus a bounded
	// unified diff (W2d); render it colored instead of an all-added dump.
	if label == "write_file" && !msg.IsError {
		if cell := renderWriteFileCell(msg.Content, msg.Duration, width); cell != "" {
			return cell
		}
	}
	// update_plan renders as a Codex-style checklist with progress, not a
	// one-line summary.
	if label == "update_plan" && !msg.IsError {
		if cell := renderPlanCell(msg.Content, msg.Duration, width); cell != "" {
			return cell
		}
	}

	// The red bullet conveys failure; keep a dim duration suffix.
	sb.WriteString(toolHeaderLine(action, false, msg.IsError, isCmd))
	if msg.Duration > 0 {
		sb.WriteString(toolBulletDim.Render(fmt.Sprintf(" %.1fs", msg.Duration)))
	}
	sb.WriteString("\n")
	if result := toolResultLine(label, msg.Content, width-6); result != "" {
		sb.WriteString("  " + glyphCorner + " " + result + "\n")
	}
	// For command tools, show a bounded head of the actual output so the run is
	// not a black box. File-editing tools get a colored diff instead.
	if block := renderCommandOutputBlock(label, msg.Content, width-4); block != "" {
		sb.WriteString(block)
	} else if !msg.IsError {
		if diff := renderToolDiff(label, args, width-4); diff != "" {
			sb.WriteString(diff)
		}
	}
	return sb.String()
}

// renderCommandOutputBlock shows a bounded head of raw command output (stdout/
// stderr) as dim lines, Codex-style. Terminal tools return plain text rather
// than JSON, so without this the transcript would only ever show the first line
// of any command's output. It renders nothing for single-line or empty output
// (the one-line summary already covers that) and only applies to command tools.
func renderCommandOutputBlock(label, content string, width int) string {
	switch label {
	case "terminal", "execute_command", "shell":
	default:
		return ""
	}
	content = strings.TrimRight(stripANSI(content), "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= 1 {
		return ""
	}
	const maxLines = 6
	var sb strings.Builder
	shown := 0
	for _, ln := range lines {
		if shown >= maxLines {
			break
		}
		sb.WriteString("  " + glyphCorner + " " + diffCtxStyle.Render(truncateToWidth(ln, width-4)) + "\n")
		shown++
	}
	if len(lines) > maxLines {
		sb.WriteString("  " + glyphCorner + " " + diffCtxStyle.Render(fmt.Sprintf("… %d more line(s)", len(lines)-maxLines)) + "\n")
	}
	return sb.String()
}

var (
	diffAddStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	diffDelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	diffCtxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// renderToolDiff shows a compact, colored diff for file-editing tools so the
// user can see what actually changed (codex-style), instead of an opaque
// "Edited with patch". It bounds the output to a handful of lines.
func renderToolDiff(label string, args map[string]interface{}, width int) string {
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
			styled = diffAddStyle.Render(truncateToWidth(ln, width-4))
		case strings.HasPrefix(ln, "-"):
			removed++
			styled = diffDelStyle.Render(truncateToWidth(ln, width-4))
		default:
			styled = diffCtxStyle.Render(truncateToWidth(ln, width-4))
		}
		if shown < maxLines {
			sb.WriteString("  " + glyphCorner + " " + styled + "\n")
			shown++
		}
	}
	if len(lines) > maxLines {
		sb.WriteString("  " + glyphCorner + " " + diffCtxStyle.Render(fmt.Sprintf("… %d more line(s)", len(lines)-maxLines)) + "\n")
	}
	if added > 0 || removed > 0 {
		sb.WriteString("  " + glyphCorner + " " + diffCtxStyle.Render(fmt.Sprintf("(+%d −%d)", added, removed)) + "\n")
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
func renderPatchCell(patch string, duration float64, width, maxLines int) string {
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
		header := fmt.Sprintf("%s %s %s (%s %s)", glyphBullet, f.verb, f.path,
			diffAddStyle.Render(fmt.Sprintf("+%d", added)), diffDelStyle.Render(fmt.Sprintf("-%d", removed)))
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
				sb.WriteString("  " + diffCtxStyle.Render("@@ "+l.text) + "\n")
			case '+':
				gutter := ""
				if isAdd {
					gutter = diffCtxStyle.Render(fmt.Sprintf("%*d ", gutterW, lineNo))
				}
				sb.WriteString("  " + gutter + diffAddStyle.Render("+ "+truncateToWidth(l.text, width-10)) + "\n")
			case '-':
				sb.WriteString("  " + diffDelStyle.Render("- "+truncateToWidth(l.text, width-10)) + "\n")
			default:
				sb.WriteString("  " + diffCtxStyle.Render("  "+truncateToWidth(l.text, width-10)) + "\n")
			}
			budget--
		}
	}
	if hidden > 0 {
		sb.WriteString("  " + diffCtxStyle.Render(fmt.Sprintf("… +%d more line(s) — open history to see the full diff", hidden)) + "\n")
	}
	return sb.String()
}

// maxWriteFileDiffPreview bounds the diff body shown inline for a write_file
// cell; the full diff is available via /history.
const maxWriteFileDiffPreview = 30

// renderWriteFileCell renders write_file's "Created/Edited <path> (+A -B)"
// header plus its colored diff body. Legacy results (e.g. "Written N bytes")
// just render as the header line. Returns "" for empty content (fallback).
func renderWriteFileCell(content string, duration float64, width int) string {
	content = strings.TrimRight(content, "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	var sb strings.Builder
	sb.WriteString(glyphBullet + " " + lines[0])
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
			styled = diffAddStyle.Render(truncateToWidth(ln, width-6))
		case strings.HasPrefix(ln, "-"):
			styled = diffDelStyle.Render(truncateToWidth(ln, width-6))
		default:
			styled = diffCtxStyle.Render(truncateToWidth(ln, width-6))
		}
		sb.WriteString("  " + styled + "\n")
		shown++
	}
	if remaining := len(lines) - 1 - shown; remaining > 0 {
		sb.WriteString("  " + diffCtxStyle.Render(fmt.Sprintf("… %d more line(s)", remaining)) + "\n")
	}
	return sb.String()
}

var (
	planDoneTextStyle = lipgloss.NewStyle().Faint(true).Strikethrough(true)             // completed: struck-through + dim
	planActiveStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true) // in-progress: cyan bold
	planPendingStyle  = lipgloss.NewStyle().Faint(true)                                 // pending: dim
	planExplStyle     = lipgloss.NewStyle().Faint(true).Italic(true)                    // explanation note
	planHeaderStyle   = lipgloss.NewStyle().Bold(true)
	planFaintStyle    = lipgloss.NewStyle().Faint(true)
)

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
func renderPlanCell(content string, duration float64, width int) string {
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
			block = append(block, planExplStyle.Render(ln))
		}
	}
	if len(payload.Plan) == 0 {
		block = append(block, planExplStyle.Render("(no steps provided)"))
	}
	shown := 0
	for _, s := range payload.Plan {
		if shown >= maxPlanSteps {
			break
		}
		block = append(block, planStepLines(strings.TrimSpace(s.Step), s.Status, width-4)...)
		shown++
	}
	if len(payload.Plan) > maxPlanSteps {
		block = append(block, planPendingStyle.Render(fmt.Sprintf("… %d more steps", len(payload.Plan)-maxPlanSteps)))
	}

	var sb strings.Builder
	sb.WriteString(planFaintStyle.Render(glyphBullet) + " " + planHeaderStyle.Render("Updated plan") +
		planFaintStyle.Render(fmt.Sprintf(" · %d/%d", completed, len(payload.Plan))) + "\n")
	// Tree prefix: first block line gets "  └ ", the rest a flat 4-space indent.
	for i, ln := range block {
		if i == 0 {
			sb.WriteString(planFaintStyle.Render("  └ ") + ln + "\n")
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
func planStepLines(text, status string, contentWidth int) []string {
	glyph := glyphPlanBox + " "
	glyphStyle := planPendingStyle
	textStyle := planPendingStyle
	switch status {
	case "completed":
		glyph = glyphPlanDone + " "
		glyphStyle = planFaintStyle
		textStyle = planDoneTextStyle
	case "in_progress":
		glyphStyle = planActiveStyle
		textStyle = planActiveStyle
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

// noticeStyleFor colors a compact one-line notice by its leading glyph so the
// transcript record of an approval (requested / approved / denied) reads at a
// glance without a multi-line cell.
func noticeStyleFor(content string) lipgloss.Style {
	switch {
	case strings.HasPrefix(content, glyphWarning):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber: attention
	case strings.HasPrefix(content, glyphCheck):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green: approved
	case strings.HasPrefix(content, glyphCross):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // red: denied
	default:
		return lipgloss.NewStyle().Faint(true)
	}
}

// renderNoticeMessage renders a "notice" cell: exactly ONE compact line (long
// content is truncated), used as the durable transcript record for transient
// interactions such as approvals. The interactive detail lives in the active
// region, not in history.
func renderNoticeMessage(content string, width int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if width < 12 {
		width = 12
	}
	content = strings.ReplaceAll(content, "\n", " ")
	return noticeStyleFor(content).Render(truncateToWidth(content, width-1))
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
