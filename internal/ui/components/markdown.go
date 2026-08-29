package components

import (
	"fmt"
	"strconv"
	"strings"

	uicommon "selfmind/internal/ui/common"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	gmtext "github.com/yuin/goldmark/text"
)

var terminalMarkdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

var (
	markdownHeading1Style = lipgloss.NewStyle().Bold(true).Underline(true)
	markdownHeading2Style = lipgloss.NewStyle().Bold(true)
	markdownHeading3Style = lipgloss.NewStyle().Bold(true).Italic(true)
	markdownHeadingStyle  = lipgloss.NewStyle().Italic(true)
	markdownCodeStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteMuted))
	markdownInlineStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteBlue))
	markdownLinkStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteBlue)).Underline(true)
	markdownMutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteSubtle))
	markdownQuoteStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteGreen))
	markdownMarkerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteBlue))
	markdownTableHead     = lipgloss.NewStyle().Bold(true)
	markdownStrikeStyle   = lipgloss.NewStyle().Strikethrough(true)
)

// RenderMarkdown turns CommonMark/GFM source into terminal-safe semantic
// lines. It deliberately owns layout rather than HTML generation so callers
// retain correct terminal-cell wrapping, hanging indents, and narrow-table
// fallbacks. ANSI/control sanitization remains the caller's trust boundary.
func RenderMarkdown(source string, width int) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if width < 8 {
		width = 8
	}
	r := terminalMarkdownRenderer{
		source: []byte(source),
	}
	doc := terminalMarkdown.Parser().Parse(gmtext.NewReader(r.source))
	return strings.TrimSpace(strings.Join(r.renderBlockChildren(doc, width), "\n\n"))
}

type terminalMarkdownRenderer struct {
	source []byte
}

func (r terminalMarkdownRenderer) renderBlockChildren(parent ast.Node, width int) []string {
	blocks := make([]string, 0, 4)
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		if rendered := strings.TrimRight(r.renderBlock(node, width), "\n"); strings.TrimSpace(ansi.Strip(rendered)) != "" {
			blocks = append(blocks, rendered)
		}
	}
	return blocks
}

func (r terminalMarkdownRenderer) renderBlock(node ast.Node, width int) string {
	if width < 4 {
		width = 4
	}
	switch typed := node.(type) {
	case *ast.Paragraph:
		return wrapMarkdownText(r.renderInlines(typed), width)
	case *ast.TextBlock:
		return wrapMarkdownText(r.renderInlines(typed), width)
	case *ast.Heading:
		content := wrapMarkdownText(r.renderInlines(typed), width)
		switch typed.Level {
		case 1:
			return markdownHeading1Style.Render(content)
		case 2:
			return markdownHeading2Style.Render(content)
		case 3:
			return markdownHeading3Style.Render(content)
		default:
			return markdownHeadingStyle.Render(content)
		}
	case *ast.Blockquote:
		inner := strings.Join(r.renderBlockChildren(typed, max(4, width-2)), "\n\n")
		return prefixMarkdownLines(inner, markdownQuoteStyle.Render("│ "), markdownQuoteStyle.Render("│"))
	case *ast.List:
		return r.renderList(typed, width, "")
	case *ast.FencedCodeBlock:
		// Parse and retain the language through Goldmark even though P0 uses a
		// neutral code style. A later highlighter can consume this without
		// changing the renderer contract.
		_ = typed.Language(r.source)
		return r.renderCodeLines(typed.Lines(), width)
	case *ast.CodeBlock:
		return r.renderCodeLines(typed.Lines(), width)
	case *ast.ThematicBreak:
		return markdownMutedStyle.Render(strings.Repeat("─", min(width, 24)))
	case *extast.Table:
		return r.renderTable(typed, width)
	case *ast.HTMLBlock:
		return wrapMarkdownText(string(typed.Lines().Value(r.source)), width)
	default:
		children := r.renderBlockChildren(node, width)
		if len(children) > 0 {
			return strings.Join(children, "\n\n")
		}
		return wrapMarkdownText(r.renderInlines(node), width)
	}
}

func (r terminalMarkdownRenderer) renderList(list *ast.List, width int, indent string) string {
	lines := make([]string, 0, 4)
	itemNumber := list.Start
	if itemNumber <= 0 {
		itemNumber = 1
	}
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*ast.ListItem)
		if !ok {
			continue
		}
		marker := "•"
		if list.IsOrdered() {
			marker = strconv.Itoa(itemNumber) + string(list.Marker)
			itemNumber++
		}
		markerWidth := runewidth.StringWidth(marker) + 1
		firstPrefix := indent + markdownMarkerStyle.Render(marker) + " "
		continuation := indent + strings.Repeat(" ", markerWidth)
		firstBlock := true
		for child := listItem.FirstChild(); child != nil; child = child.NextSibling() {
			switch nested := child.(type) {
			case *ast.Paragraph:
				prefix := continuation
				if firstBlock {
					prefix = firstPrefix
				}
				wrapped := wrapMarkdownWithPrefixes(r.renderInlines(nested), width, prefix, continuation)
				if !firstBlock && !list.IsTight {
					lines = append(lines, "")
				}
				lines = append(lines, strings.Split(wrapped, "\n")...)
				firstBlock = false
			case *ast.List:
				nestedIndent := continuation
				rendered := r.renderList(nested, width, nestedIndent)
				if rendered != "" {
					lines = append(lines, strings.Split(rendered, "\n")...)
				}
				firstBlock = false
			default:
				rendered := r.renderBlock(child, max(4, width-runewidth.StringWidth(continuation)))
				if rendered == "" {
					continue
				}
				prefix := continuation
				if firstBlock {
					prefix = firstPrefix
				}
				lines = append(lines, strings.Split(prefixMarkdownLines(rendered, prefix, continuation), "\n")...)
				firstBlock = false
			}
		}
		if firstBlock {
			lines = append(lines, strings.TrimRight(firstPrefix, " "))
		}
		if !list.IsTight && item.NextSibling() != nil {
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (r terminalMarkdownRenderer) renderCodeLines(segments *gmtext.Segments, width int) string {
	if segments == nil || segments.Len() == 0 {
		return ""
	}
	lines := make([]string, 0, segments.Len())
	for i := 0; i < segments.Len(); i++ {
		segment := segments.At(i)
		line := strings.TrimSuffix(string(segment.Value(r.source)), "\n")
		wrapped := ansi.Hardwrap(line, max(1, width-2), false)
		for _, physical := range strings.Split(wrapped, "\n") {
			lines = append(lines, "  "+markdownCodeStyle.Render(physical))
		}
	}
	return strings.Join(lines, "\n")
}

func (r terminalMarkdownRenderer) renderInlines(parent ast.Node) string {
	var out strings.Builder
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		switch typed := node.(type) {
		case *ast.Text:
			out.Write(typed.Segment.Value(r.source))
			if typed.HardLineBreak() {
				out.WriteByte('\n')
			} else if typed.SoftLineBreak() {
				out.WriteByte(' ')
			}
		case *ast.String:
			out.Write(typed.Value)
		case *ast.Emphasis:
			content := r.renderInlines(typed)
			if typed.Level >= 2 {
				out.WriteString(lipgloss.NewStyle().Bold(true).Render(content))
			} else {
				out.WriteString(lipgloss.NewStyle().Italic(true).Render(content))
			}
		case *ast.CodeSpan:
			content := strings.ReplaceAll(r.renderInlines(typed), "\n", " ")
			out.WriteString(markdownInlineStyle.Render(content))
		case *ast.Link:
			label := r.renderInlines(typed)
			destination := string(typed.Destination)
			if strings.TrimSpace(label) == "" {
				label = destination
			}
			out.WriteString(markdownLinkStyle.Render(label))
			if destination != "" && strings.TrimSpace(ansi.Strip(label)) != destination {
				out.WriteString(markdownMutedStyle.Render(" (" + destination + ")"))
			}
		case *ast.AutoLink:
			out.WriteString(markdownLinkStyle.Render(string(typed.URL(r.source))))
		case *ast.Image:
			label := strings.TrimSpace(ansi.Strip(r.renderInlines(typed)))
			if label == "" {
				label = "image"
			}
			out.WriteString(markdownMutedStyle.Render("[" + label + "]"))
			if destination := string(typed.Destination); destination != "" {
				out.WriteString(markdownMutedStyle.Render(" (" + destination + ")"))
			}
		case *extast.Strikethrough:
			out.WriteString(markdownStrikeStyle.Render(r.renderInlines(typed)))
		case *extast.TaskCheckBox:
			if typed.IsChecked {
				out.WriteString("[x] ")
			} else {
				out.WriteString("[ ] ")
			}
		case *ast.RawHTML:
			out.Write(typed.Text(r.source))
		default:
			out.WriteString(r.renderInlines(typed))
		}
	}
	return out.String()
}

func (r terminalMarkdownRenderer) renderTable(table *extast.Table, width int) string {
	var headers []string
	rows := make([][]string, 0, 4)
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch typed := child.(type) {
		case *extast.TableHeader:
			headers = r.tableCells(typed)
		case *extast.TableRow:
			rows = append(rows, r.tableCells(typed))
		}
	}
	columns := len(headers)
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return ""
	}
	for len(headers) < columns {
		headers = append(headers, fmt.Sprintf("Column %d", len(headers)+1))
	}
	widths := make([]int, columns)
	for column, header := range headers {
		widths[column] = markdownCellWidth(header)
	}
	for _, row := range rows {
		for column := range columns {
			if column < len(row) {
				widths[column] = max(widths[column], markdownCellWidth(row[column]))
			}
		}
	}
	total := 3 * (columns - 1)
	for _, columnWidth := range widths {
		total += columnWidth
	}
	if total > width {
		return renderMarkdownTableRecords(headers, rows, width)
	}

	lines := []string{renderMarkdownTableRow(headers, widths, true)}
	rules := make([]string, columns)
	for column, columnWidth := range widths {
		rules[column] = strings.Repeat("─", columnWidth)
	}
	lines = append(lines, markdownMutedStyle.Render(strings.Join(rules, "─┼─")))
	for _, row := range rows {
		lines = append(lines, renderMarkdownTableRow(row, widths, false))
	}
	return strings.Join(lines, "\n")
}

func (r terminalMarkdownRenderer) tableCells(parent ast.Node) []string {
	cells := make([]string, 0, 4)
	for cell := parent.FirstChild(); cell != nil; cell = cell.NextSibling() {
		content := strings.TrimSpace(strings.ReplaceAll(r.renderInlines(cell), "\n", " "))
		cells = append(cells, content)
	}
	return cells
}

func renderMarkdownTableRow(cells []string, widths []int, header bool) string {
	parts := make([]string, len(widths))
	for column, width := range widths {
		value := ""
		if column < len(cells) {
			value = cells[column]
		}
		if column < len(widths)-1 {
			value = padMarkdownCell(value, width)
		}
		if header {
			value = markdownTableHead.Render(value)
		}
		parts[column] = value
	}
	return strings.Join(parts, markdownMutedStyle.Render(" │ "))
}

func renderMarkdownTableRecords(headers []string, rows [][]string, width int) string {
	records := make([]string, 0, len(rows))
	for _, row := range rows {
		fields := make([]string, 0, len(headers))
		for column, header := range headers {
			value := ""
			if column < len(row) {
				value = row[column]
			}
			label := strings.TrimSpace(ansi.Strip(header))
			if label == "" {
				label = fmt.Sprintf("Column %d", column+1)
			}
			prefix := markdownTableHead.Render(label + ":")
			oneLine := prefix + " " + value
			if markdownCellWidth(oneLine) <= width {
				fields = append(fields, oneLine)
				continue
			}
			fields = append(fields, prefix+"\n"+wrapMarkdownWithPrefixes(value, width, "  ", "  "))
		}
		records = append(records, strings.Join(fields, "\n"))
	}
	return strings.Join(records, "\n\n")
}

func wrapMarkdownText(value string, width int) string {
	return strings.TrimRight(ansi.Wrap(strings.TrimSpace(value), max(1, width), ""), "\n")
}

func wrapMarkdownWithPrefixes(value string, width int, firstPrefix, continuation string) string {
	prefixWidth := max(markdownCellWidth(firstPrefix), markdownCellWidth(continuation))
	available := max(1, width-prefixWidth)
	wrapped := ansi.Wrap(strings.TrimSpace(value), available, "")
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = firstPrefix + lines[i]
		} else {
			lines[i] = continuation + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func prefixMarkdownLines(value, firstPrefix, continuation string) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = firstPrefix + lines[i]
		} else if lines[i] == "" {
			lines[i] = strings.TrimRight(continuation, " ")
		} else {
			lines[i] = continuation + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func padMarkdownCell(value string, width int) string {
	padding := width - markdownCellWidth(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

func markdownCellWidth(value string) int {
	return runewidth.StringWidth(ansi.Strip(value))
}
