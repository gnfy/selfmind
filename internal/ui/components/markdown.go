package components

import (
	"strconv"
	"strings"

	uitheme "selfmind/internal/ui/theme"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var terminalMarkdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

type markdownStyles struct {
	heading1, heading2, heading3, heading lipgloss.Style
	code, inline, link, muted, quote      lipgloss.Style
	marker, tableHead, strike             lipgloss.Style
}

// MarkdownRenderer renders Markdown with an immutable semantic theme.
type MarkdownRenderer struct{ styles markdownStyles }

func NewMarkdownRenderer(t uitheme.Theme) MarkdownRenderer {
	return MarkdownRenderer{styles: markdownStyles{
		heading1:  lipgloss.NewStyle().Bold(true).Underline(true),
		heading2:  lipgloss.NewStyle().Bold(true),
		heading3:  lipgloss.NewStyle().Bold(true).Italic(true),
		heading:   lipgloss.NewStyle().Italic(true),
		code:      lipgloss.NewStyle().Foreground(t.Color(uitheme.TextSecondary)),
		inline:    lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)),
		link:      lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)).Underline(true),
		muted:     lipgloss.NewStyle().Foreground(t.Color(uitheme.TextDecorative)),
		quote:     lipgloss.NewStyle().Foreground(t.Color(uitheme.Success)),
		marker:    lipgloss.NewStyle().Foreground(t.Color(uitheme.Accent)),
		tableHead: lipgloss.NewStyle().Bold(true),
		strike:    lipgloss.NewStyle().Strikethrough(true),
	}}
}

// RenderMarkdown turns CommonMark/GFM source into terminal-safe semantic
// lines. It deliberately owns layout rather than HTML generation so callers
// retain correct terminal-cell wrapping, hanging indents, and narrow-table
// fallbacks. Where CommonMark's HTML rules and a terminal differ, line-level
// behavior follows Codex's renderer (a soft break is a line break; code lines
// wrap at words under their own indentation) rather than a design of our own.
// ANSI/control sanitization remains the caller's trust boundary.
func RenderMarkdown(source string, width int) string {
	return NewMarkdownRenderer(uitheme.Default()).Render(source, width)
}

func (m MarkdownRenderer) Render(source string, width int) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if width < 8 {
		width = 8
	}
	r := terminalMarkdownRenderer{
		source: []byte(source),
		styles: m.styles,
	}
	doc := terminalMarkdown.Parser().Parse(gmtext.NewReader(r.source))
	// Only blank lines are trimmed: the first row of a table or a code block starts
	// with the padding or gutter its other rows share.
	return strings.Trim(strings.Join(r.renderBlockChildren(doc, width), "\n\n"), "\n")
}

type terminalMarkdownRenderer struct {
	source []byte
	styles markdownStyles
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
			return r.styles.heading1.Render(content)
		case 2:
			return r.styles.heading2.Render(content)
		case 3:
			return r.styles.heading3.Render(content)
		default:
			return r.styles.heading.Render(content)
		}
	case *ast.Blockquote:
		inner := strings.Join(r.renderBlockChildren(typed, max(4, width-2)), "\n\n")
		// Every line carries the bar and a space, as Codex prints "> " on each;
		// the space sits outside the style so a blank line can drop it.
		bar := r.styles.quote.Render("│") + " "
		return prefixMarkdownLines(inner, bar, bar)
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
		return r.styles.muted.Render(strings.Repeat("─", min(width, 24)))
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
		firstPrefix := indent + r.styles.marker.Render(marker) + " "
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
		for _, physical := range wrapCodeLine(line, max(1, width-2)) {
			lines = append(lines, "  "+r.styles.code.Render(physical))
		}
	}
	return strings.Join(lines, "\n")
}

// wrapCodeLine word-wraps one code line to width cells and continues each
// wrapped row under the line's own indentation, as Codex pre-wraps history
// lines. A cut at a fixed column splits a flag such as `--confirm` across two
// rows and leaves a command nobody can read back as written; a token wider than
// the row is still broken, because a row must not overflow. An indentation wider
// than half the row is not carried, so a deeply indented line in a narrow
// terminal still makes progress.
func wrapCodeLine(line string, width int) []string {
	body := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(body)]
	if ansi.StringWidth(indent) > width/2 {
		indent, body = "", line
	}
	rows := strings.Split(wrapMarkdownWords(body, max(1, width-ansi.StringWidth(indent))), "\n")
	for i := range rows {
		rows[i] = indent + rows[i]
	}
	return rows
}

// textValue is a text node as written: a backslash before punctuation and
// character references (&amp;, &#35;) stand for the character, as Codex's parser
// delivers them. Inside a code span they are literal.
func (r terminalMarkdownRenderer) textValue(text *ast.Text) []byte {
	value := text.Segment.Value(r.source)
	if _, inCode := text.Parent().(*ast.CodeSpan); inCode {
		return value
	}
	var out []byte
	for i := 0; i < len(value); {
		if value[i] == '\\' && i+1 < len(value) && util.IsPunct(value[i+1]) {
			out = append(out, value[i+1])
			i += 2
			continue
		}
		end := i + 1
		for end < len(value) && !(value[end] == '\\' && end+1 < len(value) && util.IsPunct(value[end+1])) {
			end++
		}
		out = append(out, util.ResolveEntityNames(util.ResolveNumericReferences(value[i:end]))...)
		i = end
	}
	return out
}

func (r terminalMarkdownRenderer) renderInlines(parent ast.Node) string {
	var out strings.Builder
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		switch typed := node.(type) {
		case *ast.Text:
			out.Write(r.textValue(typed))
			// A soft break stays a line break, as in Codex's renderer. CommonMark
			// folds it into a space, which runs together the commands or items a
			// model wrote one per line. A table row ends at the newline, so a cell
			// never holds one.
			if typed.HardLineBreak() || typed.SoftLineBreak() {
				out.WriteByte('\n')
			}
		case *ast.String:
			out.Write(typed.Value)
		case *ast.Emphasis:
			content := r.renderInlines(typed)
			if typed.Level >= 2 {
				out.WriteString(styleLines(lipgloss.NewStyle().Bold(true), content))
			} else {
				out.WriteString(styleLines(lipgloss.NewStyle().Italic(true), content))
			}
		case *ast.CodeSpan:
			content := strings.ReplaceAll(r.renderInlines(typed), "\n", " ")
			out.WriteString(r.styles.inline.Render(content))
		case *ast.Link:
			label := r.renderInlines(typed)
			destination := string(typed.Destination)
			if strings.TrimSpace(label) == "" {
				label = destination
			}
			out.WriteString(styleLines(r.styles.link, label))
			if destination != "" && strings.TrimSpace(ansi.Strip(label)) != destination {
				out.WriteString(r.styles.muted.Render(" (" + destination + ")"))
			}
		case *ast.AutoLink:
			out.WriteString(r.styles.link.Render(string(typed.URL(r.source))))
		case *ast.Image:
			label := strings.TrimSpace(ansi.Strip(r.renderInlines(typed)))
			if label == "" {
				label = "image"
			}
			out.WriteString(r.styles.muted.Render("[" + label + "]"))
			if destination := string(typed.Destination); destination != "" {
				out.WriteString(r.styles.muted.Render(" (" + destination + ")"))
			}
		case *extast.Strikethrough:
			out.WriteString(styleLines(r.styles.strike, r.renderInlines(typed)))
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

func wrapMarkdownText(value string, width int) string {
	return strings.TrimRight(wrapMarkdownWords(strings.TrimSpace(value), max(1, width)), "\n")
}

// styleLines applies style to each line separately. lipgloss pads the shorter
// lines of a multi-line string to the widest, which would underline or strike
// through the blanks after a span that crosses a line break.
func styleLines(style lipgloss.Style, value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	return strings.Join(lines, "\n")
}

func wrapMarkdownWithPrefixes(value string, width int, firstPrefix, continuation string) string {
	prefixWidth := max(markdownCellWidth(firstPrefix), markdownCellWidth(continuation))
	available := max(1, width-prefixWidth)
	wrapped := wrapMarkdownWords(strings.TrimSpace(value), available)
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

func markdownCellWidth(value string) int {
	return runewidth.StringWidth(ansi.Strip(value))
}
