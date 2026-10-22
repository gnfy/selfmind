package components

import (
	"reflect"
	"strings"
	"testing"
)

func plainCells(texts ...string) []tableCell {
	cells := make([]tableCell, len(texts))
	for i, text := range texts {
		cells[i] = newTableCell(text)
	}
	return cells
}

func kinds(ks ...tableColumnKind) []tableColumnMetrics {
	metrics := make([]tableColumnMetrics, len(ks))
	for i, k := range ks {
		metrics[i] = tableColumnMetrics{kind: k}
	}
	return metrics
}

// Columns start at their natural width and shrink by priority: a token-heavy
// column gives up width before a narrative one, and a compact one is kept last
// (Codex's compute_column_widths / shrink_columns).
func TestComputeTableColumnWidthsShrinksByPriority(t *testing.T) {
	heavy := tableColumnMetrics{maxWidth: 40, kind: tableColumnTokenHeavy}
	prose := tableColumnMetrics{maxWidth: 40, kind: tableColumnNarrative}
	compact := tableColumnMetrics{maxWidth: 40, headerTokenWidth: 4, bodyTokenWidth: 40, kind: tableColumnCompact}
	tests := []struct {
		name      string
		metrics   []tableColumnMetrics
		available int
		want      []int
	}{
		{"fits as it is", []tableColumnMetrics{heavy, prose}, 80, []int{40, 40}},
		{"token-heavy first", []tableColumnMetrics{heavy, prose}, 60, []int{20, 40}},
		{"then narrative, down to its floor", []tableColumnMetrics{heavy, prose}, 40, []int{16, 24}},
		{"floors give way when they do not fit", []tableColumnMetrics{heavy, prose}, 30, []int{14, 16}},
		{"compact is kept last", []tableColumnMetrics{compact, heavy}, 40, []int{24, 16}},
		{"narrative gives up width before compact", []tableColumnMetrics{compact, prose}, 60, []int{40, 20}},
		{"the odd cell comes off the first column at the cap", []tableColumnMetrics{
			{maxWidth: 26, kind: tableColumnTokenHeavy}, {maxWidth: 26, kind: tableColumnTokenHeavy}}, 47, []int{23, 24}},
		{"the slack of a kind is evened out", []tableColumnMetrics{
			{maxWidth: 26, kind: tableColumnTokenHeavy}, {maxWidth: 46, kind: tableColumnTokenHeavy}}, 52, []int{26, 26}},
		{"a column is never narrower than the minimum width", []tableColumnMetrics{
			{maxWidth: 1, kind: tableColumnCompact}, {maxWidth: 1, kind: tableColumnCompact}}, 40, []int{3, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := computeTableColumnWidths(tc.metrics, tc.available)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("widths at %d = %v (fits %v), want %v", tc.available, got, ok, tc.want)
			}
		})
	}
	if _, ok := computeTableColumnWidths([]tableColumnMetrics{heavy, prose, compact}, 8); ok {
		t.Fatal("columns of three cells each cannot share 8 cells")
	}
	if _, ok := computeTableColumnWidths([]tableColumnMetrics{heavy, prose, compact}, 9); !ok {
		t.Fatal("columns of three cells each fit 9 cells")
	}
}

// A column keeps a preferred width before it shrinks further: prose and
// token-heavy columns keep 16 cells, a compact one its longest header token or
// body token (capped at 16), never more than its natural width and never under
// the minimum.
func TestPreferredTableColumnFloor(t *testing.T) {
	tests := []struct {
		name   string
		column tableColumnMetrics
		want   int
	}{
		{"prose keeps 16", tableColumnMetrics{maxWidth: 40, kind: tableColumnNarrative}, 16},
		{"token-heavy keeps 16", tableColumnMetrics{maxWidth: 40, kind: tableColumnTokenHeavy}, 16},
		{"never more than the natural width", tableColumnMetrics{maxWidth: 10, kind: tableColumnNarrative}, 10},
		{"compact keeps its body token", tableColumnMetrics{maxWidth: 40, headerTokenWidth: 3, bodyTokenWidth: 9, kind: tableColumnCompact}, 9},
		{"compact keeps its body token up to 16", tableColumnMetrics{maxWidth: 40, headerTokenWidth: 3, bodyTokenWidth: 30, kind: tableColumnCompact}, 16},
		{"compact keeps its header token", tableColumnMetrics{maxWidth: 30, headerTokenWidth: 10, bodyTokenWidth: 5, kind: tableColumnCompact}, 10},
		{"a long header token is kept past 16", tableColumnMetrics{maxWidth: 30, headerTokenWidth: 20, bodyTokenWidth: 5, kind: tableColumnCompact}, 20},
		{"never under the minimum", tableColumnMetrics{maxWidth: 10, headerTokenWidth: 1, bodyTokenWidth: 2, kind: tableColumnCompact}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferredTableColumnFloor(tc.column); got != tc.want {
				t.Fatalf("floor = %d, want %d", got, tc.want)
			}
		})
	}
}

// A column is token-heavy when most of its body tokens are 20 cells or wider,
// narrative when its cells average four words or 28 cells, and compact otherwise.
func TestCollectTableColumnMetricsClassifiesColumns(t *testing.T) {
	header := plainCells("Path", "Why", "State")
	rows := [][]tableCell{
		plainCells("internal/gateway/cli/run_view.go", "The page now names its keys and goes back", "ok"),
		plainCells("internal/ui/components/pager.go", "A footer that never scrolls away", "failed"),
	}
	metrics := collectTableColumnMetrics(header, rows, 3)
	if metrics[0].kind != tableColumnTokenHeavy || metrics[1].kind != tableColumnNarrative || metrics[2].kind != tableColumnCompact {
		t.Fatalf("kinds = %v %v %v", metrics[0].kind, metrics[1].kind, metrics[2].kind)
	}
	if metrics[0].maxWidth != 32 || metrics[0].headerTokenWidth != 4 || metrics[0].bodyTokenWidth != 32 {
		t.Fatalf("path metrics = %+v", metrics[0])
	}
	if metrics[2].maxWidth != 6 || metrics[2].bodyTokenWidth != 6 {
		t.Fatalf("state metrics = %+v", metrics[2])
	}

	// A column of long words is narrative by width, not by word count.
	wide := collectTableColumnMetrics(plainCells("Note"), [][]tableCell{plainCells("one sentence of twenty-eight cells")}, 1)
	if wide[0].kind != tableColumnNarrative {
		t.Fatalf("a 34-cell cell is %v", wide[0].kind)
	}
	// Three words and exactly 28 cells on average is prose; 27 is not.
	if k := collectTableColumnMetrics(plainCells("n"), [][]tableCell{plainCells("aaaaaaaaa bbbbbbbbb cccccccc")}, 1)[0].kind; k != tableColumnNarrative {
		t.Fatalf("a 28-cell cell of three words made the column %v", k)
	}
	if k := collectTableColumnMetrics(plainCells("n"), [][]tableCell{plainCells("aaaaaaaaa bbbbbbbbb ccccccc")}, 1)[0].kind; k != tableColumnCompact {
		t.Fatalf("a 27-cell cell of three words made the column %v", k)
	}
	// Tokens of 19 cells are not long; 20 are.
	if k := collectTableColumnMetrics(plainCells("a"), [][]tableCell{plainCells(strings.Repeat("x", 19))}, 1)[0].kind; k != tableColumnCompact {
		t.Fatalf("a 19-cell token made the column %v", k)
	}
	if k := collectTableColumnMetrics(plainCells("a"), [][]tableCell{plainCells(strings.Repeat("x", 20))}, 1)[0].kind; k != tableColumnTokenHeavy {
		t.Fatalf("a 20-cell token made the column %v", k)
	}
	// Half of the body tokens being long is already token-heavy; fewer is not.
	half := [][]tableCell{plainCells(strings.Repeat("x", 20)), plainCells("y")}
	if k := collectTableColumnMetrics(plainCells("a"), half, 1)[0].kind; k != tableColumnTokenHeavy {
		t.Fatalf("1 long token in 2 made the column %v", k)
	}
	fewer := [][]tableCell{plainCells(strings.Repeat("x", 20)), plainCells("y"), plainCells("z")}
	if k := collectTableColumnMetrics(plainCells("a"), fewer, 1)[0].kind; k != tableColumnCompact {
		t.Fatalf("1 long token in 3 made the column %v", k)
	}
	// With no body rows the header decides: four words make prose.
	if k := collectTableColumnMetrics(plainCells("one two three four"), nil, 1)[0].kind; k != tableColumnNarrative {
		t.Fatalf("a four-word header alone made the column %v", k)
	}
	if k := collectTableColumnMetrics(plainCells("one two three"), nil, 1)[0].kind; k != tableColumnCompact {
		t.Fatalf("a three-word header alone made the column %v", k)
	}
}

// Records replace the grid once enough rows hold a value the grid can no longer
// show whole, or two expansive cells in a row become tall strips: one row is
// enough for a single-row table, otherwise a third of the rows (at least two).
func TestShouldRenderTableRecordsThresholds(t *testing.T) {
	compactMetrics := kinds(tableColumnCompact, tableColumnCompact)
	widths := []int{8, 8}
	short, long := plainCells("ok", "fine"), plainCells("averyverylongvalue", "fine")
	tests := []struct {
		name string
		rows [][]tableCell
		want bool
	}{
		{"no rows", nil, false},
		{"one row, nothing fragmented", [][]tableCell{short}, false},
		{"one row, fragmented", [][]tableCell{long}, true},
		{"three rows, one fragmented", [][]tableCell{short, long, short}, false},
		{"three rows, two fragmented", [][]tableCell{short, long, long}, true},
		{"four rows, one fragmented", [][]tableCell{short, long, short, short}, false},
		{"four rows, two fragmented", [][]tableCell{short, long, long, short}, true},
		{"seven rows, two fragmented", [][]tableCell{short, long, long, short, short, short, short}, false},
		{"seven rows, three fragmented", [][]tableCell{short, long, long, long, short, short, short}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRenderTableRecords(tc.rows, widths, compactMetrics); got != tc.want {
				t.Fatalf("records = %v, want %v", got, tc.want)
			}
		})
	}
	// A token-heavy column only counts as fragmented while it is narrower than 12.
	heavy := kinds(tableColumnTokenHeavy)
	if !shouldRenderTableRecords([][]tableCell{plainCells("abcdefghijklmnopqrst")}, []int{11}, heavy) {
		t.Fatal("a token cut in an 11-cell token-heavy column should switch to records")
	}
	if shouldRenderTableRecords([][]tableCell{plainCells("abcdefghijklmnopqrst")}, []int{12}, heavy) {
		t.Fatal("a 12-cell token-heavy column is wide enough to stay a grid")
	}
	// Prose is allowed to wrap and is never fragmented by itself.
	if shouldRenderTableRecords([][]tableCell{plainCells("abcdefghijklmnopqrst")}, []int{5}, kinds(tableColumnNarrative)) {
		t.Fatal("a single narrow narrative cell is not a reason for records")
	}
}

// Expansive cells are starved when two of them in one row wrap to four lines or
// more, or a narrative cell narrower than 12 cells needs seven.
func TestShouldRenderTableRecordsWhenExpansiveCellsAreStarved(t *testing.T) {
	metrics := kinds(tableColumnNarrative, tableColumnNarrative)
	prose := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen"
	row := [][]tableCell{plainCells(prose, prose)}
	if !shouldRenderTableRecords(row, []int{20, 20}, metrics) {
		t.Fatal("two cells of four or more lines should switch to records")
	}
	if shouldRenderTableRecords([][]tableCell{plainCells(prose, "short")}, []int{20, 20}, metrics) {
		t.Fatal("one tall cell is not enough")
	}
	if shouldRenderTableRecords(row, []int{80, 80}, metrics) {
		t.Fatal("cells that fit in one line are not starved")
	}
	if !shouldRenderTableRecords([][]tableCell{plainCells(prose, "short")}, []int{11, 20}, metrics) {
		t.Fatal("a narrative cell narrower than 12 cells and seven lines tall should switch to records")
	}
	if shouldRenderTableRecords([][]tableCell{plainCells(prose, "short")}, []int{12, 20}, metrics) {
		t.Fatal("a 12-cell narrative column is not catastrophic")
	}
	// Three lines are not cramped; four are. Six lines in a column under 12 cells
	// are not catastrophic; seven are, and only in a narrative column.
	three := "one two three four five six seven eight nine ten"
	if shouldRenderTableRecords([][]tableCell{plainCells(three, three)}, []int{20, 20}, metrics) {
		t.Fatal("two cells of three lines are not starved")
	}
	words := func(n int) string { return strings.TrimSpace(strings.Repeat("aaaaaaaaaa ", n)) }
	if !shouldRenderTableRecords([][]tableCell{plainCells(words(7), "short")}, []int{11, 20}, metrics) {
		t.Fatal("seven lines in an 11-cell narrative column should switch to records")
	}
	if shouldRenderTableRecords([][]tableCell{plainCells(words(6), "short")}, []int{11, 20}, metrics) {
		t.Fatal("six lines in an 11-cell narrative column are not catastrophic")
	}
	if shouldRenderTableRecords([][]tableCell{plainCells(words(7))}, []int{11}, kinds(tableColumnTokenHeavy)) {
		t.Fatal("height alone does not starve a token-heavy column")
	}
	if shouldRenderTableRecords([][]tableCell{plainCells(prose, prose)}, []int{20, 20}, kinds(tableColumnCompact, tableColumnCompact)) {
		t.Fatal("compact columns are not judged by their height")
	}
}

// Codex's is_spillover_row, written for a parser more lenient than ours: a row
// whose only text is in its first cell is prose when its line has no table pipes,
// when it looks like HTML, or when it is a label that introduces HTML.
func TestIsSpilloverTableRow(t *testing.T) {
	row := func(first string, ownCells int, pipes bool) tableBodyRow {
		return tableBodyRow{cells: plainCells(first, ""), ownCells: ownCells, pipeSyntax: pipes}
	}
	html := &tableBodyRow{cells: plainCells("<div class=\"x\">hi</div>", ""), ownCells: 2, pipeSyntax: true}
	tests := []struct {
		name string
		row  tableBodyRow
		next *tableBodyRow
		want bool
	}{
		{"a line with no pipes", row("Note: prose", 1, false), nil, true},
		{"the same line with pipes is a row", row("Note: prose", 1, true), nil, false},
		{"two own cells, no leading pipe, is a row", row("Note", 2, false), nil, false},
		{"an empty row", row("", 1, false), nil, false},
		{"text in a later cell too", tableBodyRow{cells: plainCells("a", "b"), ownCells: 1, pipeSyntax: false}, nil, false},
		{"html content", row("<b>bold</b>", 2, true), nil, true},
		{"a closing tag", row("</div>", 2, true), nil, true},
		{"a comment-like tag", row("<!doctype x>", 2, true), nil, true},
		{"a less-than sign is not html", row("a < b and c > d", 2, true), nil, false},
		{"an opened tag with no closing bracket is not html", row("x <y and more", 2, true), nil, false},
		{"a label before html", row("HTML block:", 2, true), html, true},
		{"a label before a plain row", row("Totals:", 2, true), &tableBodyRow{cells: plainCells("x", ""), ownCells: 2, pipeSyntax: true}, false},
		{"a trailing html label", row("HTML block:", 2, true), nil, true},
		{"a trailing label that is not about html", row("Totals:", 2, true), nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSpilloverTableRow(tc.row, tc.next); got != tc.want {
				t.Fatalf("spillover = %v, want %v", got, tc.want)
			}
		})
	}
}

// Records put the header names down the left when there is room for a useful
// value beside them (12 cells, or 24 beside prose), and stack them above their
// values when there is not (Codex's table_key_value.rs). Two prose cells that wrap
// to four lines starve the grid, so these rows become records.
func TestTableRecordsAreAlignedOnlyWhenThereIsRoomForTheValue(t *testing.T) {
	why := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen"
	next := "fifteen sixteen seventeen eighteen nineteen twenty twenty-one twenty-two twenty-three"
	source := "| Name | Why | Next |\n| --- | --- | --- |\n| alpha | " + why + " | " + next + " |\n| beta | " + why + " | " + next + " |\n"

	aligned := renderPlain(source, 60)
	assertMarkdownWidth(t, aligned, 60)
	lines := strings.Split(aligned, "\n")
	rule := strings.Repeat("─", 60)
	if lines[0] != " Name  alpha" || lines[1] != " Why   one two three four five six seven eight nine ten" {
		t.Fatalf("aligned records start with %q", lines[:2])
	}
	rules, labels := 0, map[string]int{}
	for _, line := range lines {
		switch {
		case line == rule:
			rules++
		case strings.HasPrefix(line, " Name  "), strings.HasPrefix(line, " Why   "), strings.HasPrefix(line, " Next  "):
			labels[strings.Fields(line)[0]]++
		default:
			if !strings.HasPrefix(line, "       ") || strings.TrimSpace(line) == "" {
				t.Fatalf("a continuation line is not indented under the value: %q", line)
			}
		}
	}
	if rules != 1 || labels["Name"] != 2 || labels["Why"] != 2 || labels["Next"] != 2 {
		t.Fatalf("records = rules %d labels %v:\n%s", rules, labels, aligned)
	}

	// 1 + 4 (the longest header) + 2 + 24 (prose beside the labels) = 31 is the
	// narrowest aligned row; one cell less and the names stack above their values.
	if got := renderPlain(source, 31); !strings.HasPrefix(got, " Name  alpha\n Why   ") {
		t.Fatalf("31 cells should still align:\n%s", got)
	}
	stacked := renderPlain(source, 30)
	assertMarkdownWidth(t, stacked, 30)
	if !strings.HasPrefix(stacked, " Name\n  alpha\n Why\n  one two three four five six\n") {
		t.Fatalf("30 cells should stack:\n%s", stacked)
	}
}

// Beside compact values the narrowest aligned row is 1 + the longest header + 2 +
// 12 cells.
func TestTableRecordsBesideCompactValuesAlignFrom18Cells(t *testing.T) {
	// 19 cells: a 20-cell token would make the column token-heavy, which asks for 24.
	source := "| Key | Val |\n|---|---|\n| averylongidentifier | x |\n"
	if got := renderPlain(source, 18); !strings.HasPrefix(got, " Key  ") || strings.HasPrefix(got, " Key\n") {
		t.Fatalf("18 cells should align:\n%s", got)
	}
	if got := renderPlain(source, 17); !strings.HasPrefix(got, " Key\n  ") {
		t.Fatalf("17 cells should stack:\n%s", got)
	}
}

// A table of one column keeps a following line as a row of its own, as Codex does
// (its spillover rule is for tables of several columns).
func TestRenderMarkdownSingleColumnTableKeepsItsLastLine(t *testing.T) {
	got := renderPlain("| a |\n|---|\n| b |\nNote: x\n", 60)
	want := " a\n━━━━━━━━━\n b\n─────────\n Note: x"
	if got != want {
		t.Fatalf("single column table =\n%s\nwant\n%s", got, want)
	}
}

// Cells wrap with the same word rules as prose: a flag in a cell is not cut at its
// hyphens.
func TestRenderMarkdownTableCellsWrapLikeProse(t *testing.T) {
	got := renderPlain("| Cmd | Note |\n|---|---|\n| run | ab --result for the build step |\n", 24)
	assertMarkdownWidth(t, got, 24)
	for _, line := range strings.Split(got, "\n") {
		if strings.HasSuffix(line, " --") {
			t.Fatalf("a flag was cut at its hyphens: %q\n%s", line, got)
		}
	}
	if !strings.Contains(got, "--result") {
		t.Fatalf("the flag is gone:\n%s", got)
	}
}

// Cells align left, center and right inside their column; an odd remainder puts
// the extra cell on the right.
func TestRenderTableRowAlignsCells(t *testing.T) {
	sp := func(n int) string { return strings.Repeat(" ", n) }
	got := strings.Split(renderPlain("| Left | Center | Right |\n|:--|:--:|--:|\n| abcdefgh | x | y |\n| a | bcdefghij | klmnopqr |\n", 80), "\n")
	// Column widths are 8, 9 and 8; a column is followed by its padding, the gap
	// and the padding of the next column (1 + 2 + 1 = 4 cells).
	want := map[int]string{
		0: " Left" + sp(9) + "Center" + sp(9) + "Right",
		2: " abcdefgh" + sp(8) + "x" + sp(15) + "y",
		4: " a" + sp(11) + "bcdefghij" + sp(4) + "klmnopqr",
	}
	for line, text := range want {
		if got[line] != text {
			t.Fatalf("row line %d = %q, want %q", line, got[line], text)
		}
	}
}

// A cell wraps with the word rules of prose: a flag is not cut at its hyphens
// ("ab" and "--result" rather than "ab --" and "result"), and an empty cell is one
// empty line.
func TestTableCellWrap(t *testing.T) {
	if got := newTableCell("ab --result").wrap(10); !reflect.DeepEqual(got, []string{"ab", "--result"}) {
		t.Fatalf("wrap = %q", got)
	}
	if got := newTableCell("").wrap(10); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("an empty cell wraps to %q", got)
	}
	if got := (tableCell{}).wrap(10); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("a cell with no lines wraps to %q", got)
	}
}

// A row of empty cells is an empty line, not a line of blanks.
func TestRenderTableEmptyRowIsAnEmptyLine(t *testing.T) {
	got := strings.Split(renderPlain("| a | b |\n|---|---|\n| | |\n| 1 | 2 |\n", 40), "\n")
	if len(got) != 5 || got[2] != "" {
		t.Fatalf("lines = %q", got)
	}
}

// A header name wider than the record is wrapped to the record's width, under the
// one-cell margin of the labels.
func TestStackedTableRecordWrapsALongHeaderName(t *testing.T) {
	got := renderPlain("| averyveryverylongheadername | b |\n|---|---|\n| x | y |\n", 10)
	assertMarkdownWidth(t, got, 10)
	if got != " averyvery\n verylongh\n eadername\n  x\n b\n  y" {
		t.Fatalf("stacked label =\n%s", got)
	}
}

// A line with no text in a column leaves that column and the ones after it out:
// nothing in the table carries trailing blanks.
func TestRenderTableRowsCarryNoTrailingBlanks(t *testing.T) {
	got := renderPlain("| Name | Notes | Extra |\n| --- | --- | --- |\n| a-very-long-name-here | some words to wrap around inside the cell | x |\n", 44)
	assertNoTrailingBlanks(t, got)
	assertMarkdownWidth(t, got, 44)
}
