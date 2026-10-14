package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
)

// Table layout follows Codex's renderer (codex-rs/tui/src/markdown_render.rs and
// markdown_render/table_key_value.rs): aligned columns with a heavy rule under the
// header and a light rule between rows, cells wrapped inside their columns, and
// key/value records when the grid would shatter values or starve prose columns.
// The thresholds below are Codex's, not ours.
const (
	tableColumnGap      = 2
	tableCellPadding    = 1
	tableMinColumnWidth = 3
	tableHeaderRule     = "━"
	tableBodyRule       = "─"

	tableFieldLeadingPadding           = 1
	tableFieldGap                      = 2
	tableMinAlignedCompactValueWidth   = 12
	tableMinAlignedExpansiveValueWidth = 24
	tableMinScannableNarrativeWidth    = 12
	tableMinScannableTokenHeavyWidth   = 12
	tableCrampedExpansiveCellLines     = 4
	tableCatastrophicNarrativeLines    = 7
	tableStackedValueIndent            = 2
)

// tableCell is one table cell: its styled lines, split where the cell has a hard
// break.
type tableCell struct{ lines []string }

func newTableCell(rendered string) tableCell {
	return tableCell{lines: strings.Split(strings.TrimSpace(rendered), "\n")}
}

// plain is the cell's text without styling, lines joined by a space.
func (c tableCell) plain() string { return ansi.Strip(strings.Join(c.lines, " ")) }

func (c tableCell) width() int {
	widest := 0
	for _, line := range c.lines {
		widest = max(widest, ansi.StringWidth(line))
	}
	return widest
}

// wrap wraps each line of the cell to width, keeping its inline styling.
func (c tableCell) wrap(width int) []string {
	var wrapped []string
	for _, line := range c.lines {
		wrapped = append(wrapped, strings.Split(wrapMarkdownWords(line, max(1, width)), "\n")...)
	}
	if len(wrapped) == 0 {
		return []string{""}
	}
	return wrapped
}

// tableBodyRow keeps what is needed to tell a row of the table from a paragraph
// that the parser absorbed into it.
type tableBodyRow struct {
	cells        []tableCell
	ownCells     int  // cells the source line actually had; the parser pads the rest
	pipeSyntax   bool // the line starts or ends with a pipe
	hasOwnSource bool
}

type tableColumnKind int

// Compact columns hold short values (counts, status words) and resist wrapping;
// narrative columns hold prose; token-heavy columns hold long tokens (paths, URLs,
// hashes) and give up width first.
const (
	tableColumnCompact tableColumnKind = iota
	tableColumnNarrative
	tableColumnTokenHeavy
)

type tableColumnMetrics struct {
	maxWidth         int
	headerTokenWidth int
	bodyTokenWidth   int
	kind             tableColumnKind
}

func (r terminalMarkdownRenderer) renderTable(table *extast.Table, width int) string {
	columns := len(table.Alignments)
	if columns == 0 {
		return ""
	}
	var header []tableCell
	var parsed []tableBodyRow
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch typed := child.(type) {
		case *extast.TableHeader:
			header = r.tableRowCells(typed)
		case *extast.TableRow:
			parsed = append(parsed, r.tableBodyRow(typed))
		}
	}

	// The parser takes a line that follows a table, with no blank line, for one of
	// its rows. Such a line is prose and is shown after the grid.
	var spillover []tableCell
	rows := make([][]tableCell, 0, len(parsed))
	for i, row := range parsed {
		var next *tableBodyRow
		if i+1 < len(parsed) {
			next = &parsed[i+1]
		}
		if columns > 1 && isSpilloverTableRow(row, next) {
			spillover = append(spillover, row.cells[0])
			continue
		}
		rows = append(rows, normalizeTableRow(row.cells, columns))
	}
	header = normalizeTableRow(header, columns)

	metrics := collectTableColumnMetrics(header, rows, columns)
	reserved := (columns-1)*tableColumnGap + columns*tableCellPadding*2
	widths, fits := computeTableColumnWidths(metrics, max(0, width-reserved))

	var lines []string
	switch {
	case !fits && len(rows) > 0, fits && shouldRenderTableRecords(rows, widths, metrics):
		lines = r.renderTableRecords(header, rows, metrics, width)
	case !fits:
		lines = renderTablePipeFallback(header, rows, table.Alignments)
	default:
		lines = r.renderTableGrid(header, rows, widths, table.Alignments)
	}
	for _, cell := range spillover {
		for _, line := range cell.lines {
			lines = append(lines, strings.Split(wrapMarkdownText(line, width), "\n")...)
		}
	}
	return strings.Join(lines, "\n")
}

func (r terminalMarkdownRenderer) tableRowCells(row ast.Node) []tableCell {
	cells := make([]tableCell, 0, 4)
	for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
		cells = append(cells, newTableCell(r.renderInlines(cell)))
	}
	return cells
}

func (r terminalMarkdownRenderer) tableBodyRow(row ast.Node) tableBodyRow {
	parsed := tableBodyRow{cells: r.tableRowCells(row), pipeSyntax: true}
	for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
		if cell.Lines().Len() > 0 {
			parsed.ownCells++
		}
	}
	if line, ok := r.tableRowSourceLine(row); ok {
		line = strings.TrimSpace(line)
		parsed.pipeSyntax = strings.HasPrefix(line, "|") || strings.HasSuffix(line, "|")
	}
	return parsed
}

// tableRowSourceLine finds the source line of a row from the first text in it. A
// row with no text at all has no line to find, which only happens for rows of
// empty cells, and those carry pipes.
func (r terminalMarkdownRenderer) tableRowSourceLine(row ast.Node) (string, bool) {
	start := -1
	_ = ast.Walk(row, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if text, ok := node.(*ast.Text); ok && entering && start < 0 {
			start = text.Segment.Start
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	if start < 0 {
		return "", false
	}
	from, to := start, start
	for from > 0 && r.source[from-1] != '\n' {
		from--
	}
	for to < len(r.source) && r.source[to] != '\n' {
		to++
	}
	return string(r.source[from:to]), true
}

func normalizeTableRow(row []tableCell, columns int) []tableCell {
	normalized := make([]tableCell, columns)
	for i := range normalized {
		if i < len(row) {
			normalized[i] = row[i]
		} else {
			normalized[i] = tableCell{}
		}
	}
	return normalized
}

// isSpilloverTableRow detects a row that is really prose: its only non-empty cell
// is the first one and the line has no table pipes, or it holds HTML, or it is
// an HTML-introducing label (Codex's heuristic, written for a more lenient
// parser than ours; it catches what remains).
func isSpilloverTableRow(row tableBodyRow, next *tableBodyRow) bool {
	first, ok := firstNonEmptyOnlyText(row.cells)
	if !ok {
		return false
	}
	if row.ownCells <= 1 && !row.pipeSyntax {
		return true
	}
	if looksLikeHTMLContent(first) {
		return true
	}
	if strings.HasSuffix(strings.TrimRight(first, " \t"), ":") {
		if next != nil {
			if text, ok := firstNonEmptyOnlyText(next.cells); ok && looksLikeHTMLContent(text) {
				return true
			}
		}
		if next == nil && looksLikeHTMLLabelLine(first) {
			return true
		}
	}
	return false
}

func firstNonEmptyOnlyText(row []tableCell) (string, bool) {
	if len(row) == 0 {
		return "", false
	}
	first := row[0].plain()
	if strings.TrimSpace(first) == "" {
		return "", false
	}
	for _, cell := range row[1:] {
		if strings.TrimSpace(cell.plain()) != "" {
			return "", false
		}
	}
	return first, true
}

func looksLikeHTMLContent(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		tag := i + 1
		if tag < len(text) && (text[tag] == '/' || text[tag] == '!') {
			tag++
		}
		if tag < len(text) && isASCIILetter(text[tag]) && strings.Contains(text[tag+1:], ">") {
			return true
		}
	}
	return false
}

func looksLikeHTMLLabelLine(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasSuffix(trimmed, ":") {
		return false
	}
	for _, word := range strings.Fields(strings.TrimSpace(strings.TrimRight(trimmed, ":"))) {
		if strings.EqualFold(word, "html") {
			return true
		}
	}
	return false
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func collectTableColumnMetrics(header []tableCell, rows [][]tableCell, columns int) []tableColumnMetrics {
	metrics := make([]tableColumnMetrics, columns)
	for column := range metrics {
		headerPlain := header[column].plain()
		maxWidth := header[column].width()
		bodyTokenWidth, bodyTokens, longBodyTokens := 0, 0, 0
		totalWords, totalCells, totalCellWidth := 0, 0, 0
		for _, row := range rows {
			maxWidth = max(maxWidth, row[column].width())
			plain := row[column].plain()
			words := 0
			for _, token := range strings.Fields(plain) {
				tokenWidth := ansi.StringWidth(token)
				bodyTokenWidth = max(bodyTokenWidth, tokenWidth)
				if tokenWidth >= 20 {
					longBodyTokens++
				}
				words++
			}
			if words > 0 {
				bodyTokens += words
				totalWords += words
				totalCells++
				totalCellWidth += ansi.StringWidth(plain)
			}
		}
		avgWords := float64(len(strings.Fields(headerPlain)))
		avgCellWidth := float64(ansi.StringWidth(headerPlain))
		if totalCells > 0 {
			avgWords = float64(totalWords) / float64(totalCells)
			avgCellWidth = float64(totalCellWidth) / float64(totalCells)
		}
		kind := tableColumnCompact
		switch {
		case longBodyTokens > 0 && longBodyTokens >= max(0, bodyTokens-longBodyTokens):
			kind = tableColumnTokenHeavy
		case avgWords >= 4 || avgCellWidth >= 28:
			kind = tableColumnNarrative
		}
		metrics[column] = tableColumnMetrics{
			maxWidth:         maxWidth,
			headerTokenWidth: longestTokenWidth(headerPlain),
			bodyTokenWidth:   bodyTokenWidth,
			kind:             kind,
		}
	}
	return metrics
}

func longestTokenWidth(text string) int {
	widest := 0
	for _, token := range strings.Fields(text) {
		widest = max(widest, ansi.StringWidth(token))
	}
	return widest
}

// computeTableColumnWidths gives each column its natural width, then shrinks the
// columns by priority until they fit: token-heavy columns give up width before
// narrative prose, and compact columns are kept last. It reports false when even
// the minimum width of every column does not fit.
func computeTableColumnWidths(metrics []tableColumnMetrics, available int) ([]int, bool) {
	widths := make([]int, len(metrics))
	floors := make([]int, len(metrics))
	for i, column := range metrics {
		widths[i] = max(column.maxWidth, tableMinColumnWidth)
		floors[i] = preferredTableColumnFloor(column)
	}
	if available < len(metrics)*tableMinColumnWidth {
		return nil, false
	}
	if floorTotal := sumInts(floors); floorTotal > available {
		minimums := make([]int, len(floors))
		for i := range minimums {
			minimums[i] = tableMinColumnWidth
		}
		shrinkTableColumns(floors, minimums, metrics, floorTotal-available)
	}
	if total := sumInts(widths); total > available {
		if remaining := shrinkTableColumns(widths, floors, metrics, total-available); remaining > 0 {
			return nil, false
		}
	}
	return widths, true
}

// preferredTableColumnFloor is the width a column keeps before it shrinks further:
// prose and token-heavy columns keep a readable 16 cells; a compact column keeps
// its longest header token or body token (capped at 16).
func preferredTableColumnFloor(column tableColumnMetrics) int {
	target := 16
	if column.kind == tableColumnCompact {
		target = max(column.headerTokenWidth, min(column.bodyTokenWidth, 16))
	}
	return min(max(target, tableMinColumnWidth), column.maxWidth)
}

// shrinkTableColumns removes amount cells from the columns above their floors, in
// priority order, evening out the slack within a kind. It returns what it could
// not remove.
func shrinkTableColumns(widths, floors []int, metrics []tableColumnMetrics, amount int) int {
	slack := func(i int) int { return max(0, widths[i]-floors[i]) }
	for _, kind := range []tableColumnKind{tableColumnTokenHeavy, tableColumnNarrative, tableColumnCompact} {
		slackTotal, widest := 0, 0
		for i := range widths {
			if metrics[i].kind == kind {
				slackTotal += slack(i)
				widest = max(widest, slack(i))
			}
		}
		toRemove := min(amount, slackTotal)
		if toRemove == 0 {
			continue
		}
		low, high := 0, widest
		for low < high {
			limit := low + (high-low)/2
			removed := 0
			for i := range widths {
				if metrics[i].kind == kind {
					removed += max(0, slack(i)-limit)
				}
			}
			if removed > toRemove {
				low = limit + 1
			} else {
				high = limit
			}
		}
		limit, removed := low, 0
		for i := range widths {
			if metrics[i].kind == kind {
				reduction := max(0, slack(i)-limit)
				widths[i] -= reduction
				removed += reduction
			}
		}
		for i := range widths {
			if toRemove-removed == 0 {
				break
			}
			if metrics[i].kind == kind && slack(i) == limit {
				widths[i]--
				removed++
			}
		}
		amount -= toRemove
		if amount == 0 {
			break
		}
	}
	return amount
}

func sumInts(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func (r terminalMarkdownRenderer) renderTableGrid(header []tableCell, rows [][]tableCell, widths []int, alignments []extast.Alignment) []string {
	lines := r.renderTableRow(header, widths, alignments, &r.styles.tableHead)
	lines = append(lines, r.tableRule(widths, tableHeaderRule))
	for i, row := range rows {
		lines = append(lines, r.renderTableRow(row, widths, alignments, nil)...)
		if i+1 < len(rows) {
			lines = append(lines, r.tableRule(widths, tableBodyRule))
		}
	}
	return lines
}

func (r terminalMarkdownRenderer) tableRule(widths []int, rule string) string {
	segments := make([]string, len(widths))
	for i, width := range widths {
		segments[i] = strings.Repeat(rule, width+tableCellPadding*2)
	}
	return r.styles.muted.Render(strings.Join(segments, strings.Repeat(" ", tableColumnGap)))
}

// renderTableRow lays one row out as one or more physical lines. A column with
// no text on a line, and every column after it, is left out so no line carries
// trailing blanks.
func (r terminalMarkdownRenderer) renderTableRow(row []tableCell, widths []int, alignments []extast.Alignment, style *lipgloss.Style) []string {
	wrapped := make([][]string, len(row))
	height := 1
	for i, cell := range row {
		wrapped[i] = cell.wrap(widths[i])
		height = max(height, len(wrapped[i]))
	}
	lines := make([]string, 0, height)
	for line := range height {
		last := -1
		for column := range wrapped {
			if line < len(wrapped[column]) && ansi.StringWidth(wrapped[column][line]) > 0 {
				last = column
			}
		}
		if last < 0 {
			lines = append(lines, "")
			continue
		}
		var b strings.Builder
		for column := 0; column <= last; column++ {
			text := ""
			if line < len(wrapped[column]) {
				text = wrapped[column][line]
			}
			remaining := max(0, widths[column]-ansi.StringWidth(text))
			left, right := 0, remaining
			switch alignments[column] {
			case extast.AlignCenter:
				left, right = remaining/2, remaining-remaining/2
			case extast.AlignRight:
				left, right = remaining, 0
			}
			if style != nil && text != "" {
				text = style.Render(text)
			}
			b.WriteString(strings.Repeat(" ", tableCellPadding+left))
			b.WriteString(text)
			if column != last {
				b.WriteString(strings.Repeat(" ", right+tableCellPadding+tableColumnGap))
			}
		}
		lines = append(lines, b.String())
	}
	return lines
}

// renderTablePipeFallback shows a table that has no rows and cannot fit as raw
// pipe lines, escaping the pipes inside cells.
func renderTablePipeFallback(header []tableCell, rows [][]tableCell, alignments []extast.Alignment) []string {
	pipeLine := func(row []tableCell) string {
		var b strings.Builder
		b.WriteString("|")
		for _, cell := range row {
			b.WriteString(" ")
			b.WriteString(strings.ReplaceAll(strings.Join(cell.lines, " "), "|", "\\|"))
			b.WriteString(" |")
		}
		return b.String()
	}
	var delimiter strings.Builder
	delimiter.WriteString("|")
	for _, alignment := range alignments {
		switch alignment {
		case extast.AlignLeft:
			delimiter.WriteString(":---")
		case extast.AlignCenter:
			delimiter.WriteString(":---:")
		case extast.AlignRight:
			delimiter.WriteString("---:")
		default:
			delimiter.WriteString("---")
		}
		delimiter.WriteString("|")
	}
	lines := []string{pipeLine(header), delimiter.String()}
	for _, row := range rows {
		lines = append(lines, pipeLine(row))
	}
	return lines
}

// shouldRenderTableRecords switches to records once enough rows hold a value the
// grid can no longer show in useful chunks, or expansive cells collapse into tall
// narrow strips.
func shouldRenderTableRecords(rows [][]tableCell, widths []int, metrics []tableColumnMetrics) bool {
	if len(rows) == 0 {
		return false
	}
	affected := 0
	for _, row := range rows {
		fragmented := false
		for column, cell := range row {
			fragmentedToken := false
			for _, token := range strings.Fields(cell.plain()) {
				if ansi.StringWidth(token) > widths[column] {
					fragmentedToken = true
					break
				}
			}
			switch metrics[column].kind {
			case tableColumnCompact:
				fragmented = fragmented || fragmentedToken
			case tableColumnTokenHeavy:
				fragmented = fragmented || (widths[column] < tableMinScannableTokenHeavyWidth && fragmentedToken)
			}
		}
		if fragmented || expansiveTableCellsAreStarved(row, widths, metrics) {
			affected++
		}
	}
	threshold := 1
	if len(rows) > 1 {
		threshold = max(2, (len(rows)+2)/3)
	}
	return affected >= threshold
}

func expansiveTableCellsAreStarved(row []tableCell, widths []int, metrics []tableColumnMetrics) bool {
	cramped := 0
	for column, cell := range row {
		kind := metrics[column].kind
		if kind == tableColumnCompact {
			continue
		}
		height := len(cell.wrap(widths[column]))
		if height >= tableCrampedExpansiveCellLines {
			cramped++
		}
		if kind == tableColumnNarrative && widths[column] < tableMinScannableNarrativeWidth && height >= tableCatastrophicNarrativeLines {
			return true
		}
	}
	return cramped >= 2
}

// renderTableRecords shows each row as a record: the header names down the left,
// aligned, when there is room for a useful value beside them, and stacked above
// their values when there is not. Records are separated by a rule.
func (r terminalMarkdownRenderer) renderTableRecords(headers []tableCell, rows [][]tableCell, metrics []tableColumnMetrics, width int) []string {
	labelWidth := 0
	for _, header := range headers {
		labelWidth = max(labelWidth, ansi.StringWidth(header.plain()))
	}
	minimumValueWidth := tableMinAlignedCompactValueWidth
	for _, column := range metrics {
		if column.kind != tableColumnCompact {
			minimumValueWidth = tableMinAlignedExpansiveValueWidth
			break
		}
	}
	aligned := tableFieldLeadingPadding+labelWidth+tableFieldGap+minimumValueWidth <= width

	var lines []string
	for i, row := range rows {
		for column, header := range headers {
			if aligned {
				lines = append(lines, r.alignedTableField(header, row[column], labelWidth, width)...)
			} else {
				lines = append(lines, r.stackedTableField(header, row[column], width)...)
			}
		}
		if i+1 < len(rows) {
			lines = append(lines, r.styles.muted.Render(strings.Repeat(tableBodyRule, width)))
		}
	}
	return lines
}

func (r terminalMarkdownRenderer) alignedTableField(header, value tableCell, labelWidth, width int) []string {
	indent := tableFieldLeadingPadding + labelWidth + tableFieldGap
	valueLines := value.wrap(width - indent)
	lines := make([]string, 0, len(valueLines))
	for i, line := range valueLines {
		if i == 0 {
			label := header.plain()
			gap := labelWidth - ansi.StringWidth(label) + tableFieldGap
			lines = append(lines, strings.Repeat(" ", tableFieldLeadingPadding)+r.styles.tableHead.Render(label)+strings.Repeat(" ", gap)+line)
			continue
		}
		lines = append(lines, strings.Repeat(" ", indent)+line)
	}
	return lines
}

func (r terminalMarkdownRenderer) stackedTableField(header, value tableCell, width int) []string {
	var lines []string
	label := tableCell{lines: []string{header.plain()}}
	for _, line := range label.wrap(max(width-tableFieldLeadingPadding, 1)) {
		lines = append(lines, strings.Repeat(" ", tableFieldLeadingPadding)+r.styles.tableHead.Render(line))
	}
	for _, line := range value.wrap(max(width-tableStackedValueIndent, 1)) {
		lines = append(lines, strings.Repeat(" ", tableStackedValueIndent)+line)
	}
	return lines
}
