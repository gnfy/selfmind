package components

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

func TestRenderMarkdownCoreBlocksGolden(t *testing.T) {
	source := strings.TrimSpace(`
# Result

Paragraph with **bold**, *italic*, ~~old~~, ` + "`code`" + `, and [docs](https://example.com/docs).

> A concise note.

- First item has enough words to wrap with a hanging indent.
  - Nested item

1. Ordered item

` + "```go" + `
fmt.Println("ok")
` + "```" + `
`)

	got := plainMarkdown(RenderMarkdown(source, 44))
	want := strings.TrimSpace(`
Result

Paragraph with bold, italic, old, code, and
docs (https://example.com/docs).

│ A concise note.

• First item has enough words to wrap with a
  hanging indent.
  • Nested item

1. Ordered item

  fmt.Println("ok")
`)
	if got != want {
		t.Fatalf("rendered markdown mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	assertMarkdownWidth(t, got, 44)
	for _, marker := range []string{"# Result", "**bold**", "~~old~~", "```"} {
		if strings.Contains(got, marker) {
			t.Fatalf("rendered markdown leaked source marker %q: %q", marker, got)
		}
	}
}

// renderPlain is the rendering without styling and without any trimming: a table
// starts with the padding its other rows share.
func renderPlain(source string, width int) string { return ansi.Strip(RenderMarkdown(source, width)) }

// The tables below follow Codex's renderer. The expected text of the first, the
// fourth and the fifth is Codex's own snapshot (markdown_render_tests.rs:
// table_renders_app_style_rows_with_themed_bold_header,
// table_keeps_grid_when_only_one_compact_record_fragments_snapshot,
// table_renders_key_value_records_when_compact_fragmentation_is_systemic_snapshot).
func TestRenderMarkdownTablesMatchCodex(t *testing.T) {
	tests := []struct {
		name, source string
		width        int
		want         string
	}{
		{
			"rows with a heavy rule under the header",
			"| A | B |\n|---|---|\n| 1 | 2 |\n", 80,
			" A      B\n━━━━━  ━━━━━\n 1      2",
		},
		{
			"a column wider than the screen shrinks and wraps, a row rule between rows",
			"| Key | Date | State |\n| --- | --- | --- |\n| short | 2025-01-01 | Ready |\n| verylongidentifier | 2025-02-02 | Ready |\n| final | 2025-03-03 | Done |\n", 40,
			" Key                Date          State\n" +
				"━━━━━━━━━━━━━━━━━  ━━━━━━━━━━━━  ━━━━━━━\n" +
				" short              2025-01-01    Ready\n" +
				"─────────────────  ────────────  ───────\n" +
				" verylongidentif    2025-02-02    Ready\n" +
				" ier\n" +
				"─────────────────  ────────────  ───────\n" +
				" final              2025-03-03    Done",
		},
		{
			"key/value records when fragmentation is systemic",
			"| Key | Notes |\n| --- | --- |\n| firstlongid | A readable explanatory sentence for this row. |\n| secondlongid | Another readable explanatory sentence for this row. |\n| short | A final readable explanatory sentence for this row. |\n", 17,
			" Key\n  firstlongid\n Notes\n  A readable\n  explanatory\n  sentence for\n  this row.\n" +
				"─────────────────\n" +
				" Key\n  secondlongid\n Notes\n  Another\n  readable\n  explanatory\n  sentence for\n  this row.\n" +
				"─────────────────\n" +
				" Key\n  short\n Notes\n  A final\n  readable\n  explanatory\n  sentence for\n  this row.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderPlain(tc.source, tc.width)
			if got != tc.want {
				t.Fatalf("table at %d =\n%s\nwant\n%s", tc.width, got, tc.want)
			}
			assertMarkdownWidth(t, got, tc.width)
		})
	}
}

// Codex's table_alignment_respects_markers.
func TestRenderMarkdownTableAlignmentMarkers(t *testing.T) {
	lines := strings.Split(renderPlain("| Left | Center | Right |\n|:-----|:------:|------:|\n| a | b | c |\n", 80), "\n")
	if lines[0] != " Left    Center    Right" || lines[2] != " a         b           c" {
		t.Fatalf("alignment rows = %q", lines)
	}
}

// Codex's table_separates_logical_rows_after_wrapped_content: a rule follows the
// header and sits between rows, and none follows the last row.
func TestRenderMarkdownTableSeparatesRowsAfterWrappedContent(t *testing.T) {
	source := "| Key | Description |\n| --- | --- |\n| -v | Enable very verbose logging output for debugging |\n| -q | Quiet output |\n"
	got := renderPlain(source, 30)
	assertMarkdownWidth(t, got, 30)
	lines := strings.Split(got, "\n")
	rules, wrappedEnd := []int{}, -1
	for i, line := range lines {
		if strings.ContainsAny(line, "━─") && strings.Trim(line, "━─ ") == "" {
			rules = append(rules, i)
		}
		if strings.Contains(line, "debugging") {
			wrappedEnd = i
		}
	}
	if len(rules) != 2 || rules[1] < wrappedEnd {
		t.Fatalf("rules at %v, the wrapped row ends at %d:\n%s", rules, wrappedEnd, got)
	}
	if last := lines[len(lines)-1]; strings.ContainsAny(last, "━─") {
		t.Fatalf("a rule follows the last row: %q", last)
	}
}

// Codex's table_falls_back_to_key_value_records_if_grid_cannot_fit.
func TestRenderMarkdownTableFallsBackToRecordsWhenTheGridCannotFit(t *testing.T) {
	source := "| c1 | c2 | c3 | c4 | c5 | c6 | c7 | c8 | c9 | c10 |\n|---|---|---|---|---|---|---|---|---|---|\n| 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 |\n"
	got := renderPlain(source, 20)
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], "c1") {
		t.Fatalf("first line = %q", lines[0])
	}
	found := false
	for _, line := range lines {
		found = found || (strings.Contains(line, "c10") && strings.Contains(line, "10"))
		if strings.HasPrefix(line, "|") || strings.ContainsAny(line, "━─") {
			t.Fatalf("records were expected, found %q in\n%s", line, got)
		}
	}
	if !found {
		t.Fatalf("no c10 record line in\n%s", got)
	}
	assertMarkdownWidth(t, got, 20)
}

// Codex's table_inside_blockquote_has_quote_prefix and
// escaped_pipes_render_in_table_cells.
func TestRenderMarkdownTableInsideAQuoteAndEscapedPipes(t *testing.T) {
	quoted := strings.Split(renderPlain("> | A | B |\n> |---|---|\n> | 1 | 2 |\n", 40), "\n")
	rule := false
	for _, line := range quoted {
		if !strings.HasPrefix(line, "│ ") {
			t.Fatalf("a table row lost the quote bar: %q", line)
		}
		rule = rule || strings.Contains(line, "━━━━━  ━━━━━")
	}
	if !rule {
		t.Fatalf("no header rule in %q", quoted)
	}
	if got := renderPlain("| Col |\n| --- |\n| a \\| b |\n", 80); !strings.Contains(got, "a | b") || strings.Contains(got, "\\") {
		t.Fatalf("escaped pipe = %q", got)
	}
}

// A line that follows a table with no blank line is taken by the parser for a
// row. It is prose and is shown after the grid, as Codex shows its "spillover"
// rows; a real row with a pipe, or one with its own cells, stays in the table.
func TestRenderMarkdownTableSpilloverLineIsProseAfterTheGrid(t *testing.T) {
	got := renderPlain("| a | b |\n|---|---|\n| 1 | 2 |\nNote: the grid is above.\n", 60)
	want := " a      b\n━━━━━  ━━━━━\n 1      2\nNote: the grid is above."
	if got != want {
		t.Fatalf("spillover =\n%s\nwant\n%s", got, want)
	}
	rows := renderPlain("| a | b |\n|---|---|\n| 1 |\n| 3 | 4 |\n", 60)
	if want := " a      b\n━━━━━  ━━━━━\n 1\n─────  ─────\n 3      4"; rows != want {
		t.Fatalf("a short row =\n%s\nwant\n%s", rows, want)
	}
	if piped := renderPlain("| a | b |\n|---|---|\n| 1 | 2 |\n| only first | |\n", 60); !strings.Contains(piped, "only first") || strings.Contains(piped, "\n\n") {
		t.Fatalf("a piped row left the table:\n%s", piped)
	}
}

// A table with no rows that cannot fit is shown as its pipe lines, with the
// alignment markers kept.
func TestRenderMarkdownHeaderOnlyTablePipeFallback(t *testing.T) {
	source := "| a \\| z | b | c | d | e | f | g | h | i | j |\n|:--|--:|:-:|---|---|---|---|---|---|---|\n"
	got := renderPlain(source, 20)
	want := "| a \\| z | b | c | d | e | f | g | h | i | j |\n|:---|---:|:---:|---|---|---|---|---|---|---|"
	if got != want {
		t.Fatalf("pipe fallback =\n%s\nwant\n%s", got, want)
	}
}

// Text is shown as written: a backslash before punctuation and character
// references stand for the character, as in Codex's parser output; a code span
// keeps them literal.
func TestRenderMarkdownDecodesEscapesAndEntitiesOutsideCode(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("Hello \\*world\\* &amp; &lt;b&gt; &#35; `code \\* &amp; kept` and AT&T and a\\b", 80))
	want := "Hello *world* & <b> # code \\* &amp; kept and AT&T and a\\b"
	if got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	// A backslash before a letter is a backslash, also where a run of text begins
	// with it; one before punctuation is not, also at the end of the text.
	if got := plainMarkdown(RenderMarkdown("\\q first and C:\\dir\\file ends with \\!", 80)); got != "\\q first and C:\\dir\\file ends with !" {
		t.Fatalf("backslashes = %q", got)
	}
}

// The first row of a table or a code block keeps the padding or gutter its other
// rows share: trimming the whole document used to eat it.
func TestRenderMarkdownKeepsTheFirstRowsPadding(t *testing.T) {
	if got := renderPlain("```\nfirst\nsecond\n```", 40); got != "  first\n  second" {
		t.Fatalf("code first = %q", got)
	}
}

func TestRenderMarkdownKeepsListContinuationIndented(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("- Alpha beta gamma delta epsilon", 18))
	want := "• Alpha beta gamma\n  delta epsilon"
	if got != want {
		t.Fatalf("wrapped list = %q, want %q", got, want)
	}
	assertMarkdownWidth(t, got, 18)
}

// Codex's renderer keeps a soft break as a line break (markdown_render_tests.rs:
// paragraph_soft_break, blockquote_soft_break, tight_item_with_soft_break), so
// what a model wrote one per line stays one per line. CommonMark's HTML rule
// folds it into a space and ran three commands together into one paragraph.
func TestRenderMarkdownSoftBreaksStayLineBreaks(t *testing.T) {
	tests := []struct{ name, source, want string }{
		{"paragraph", "Hello\nWorld", "Hello\nWorld"},
		{"space before the break", "Hello \nWorld", "Hello\nWorld"},
		{"hard break", "Hello  \nWorld", "Hello\nWorld"},
		{"blockquote", "> This is a blockquote\nwith a soft break", "│ This is a blockquote\n│ with a soft break"},
		{"tight list item", "- item line1\n  item line2", "• item line1\n  item line2"},
		{"ordered list item", "1. first\n   second", "1. first\n   second"},
		{"emphasis across the break", "**bold\nstill bold**", "bold\nstill bold"},
		{"italic across the break", "*it\nalic*", "it\nalic"},
		{"strikethrough across the break", "~~old\nstill old~~", "old\nstill old"},
		{"link label across the break", "[the documentation\nhere](https://example.com)", "the documentation\nhere (https://example.com)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := plainMarkdown(RenderMarkdown(tc.source, 60))
			if got != tc.want {
				t.Fatalf("rendered %q = %q, want %q", tc.source, got, tc.want)
			}
			assertNoTrailingBlanks(t, got)
		})
	}
}

// The bar of a quote is on every row, and a blank row between paragraphs keeps
// the bar without a trailing space.
func TestRenderMarkdownBlockquoteBarOnEveryRow(t *testing.T) {
	// With color the bar is wrapped in style codes, which is where a blank row
	// could keep a stray trailing space.
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	got := plainMarkdown(RenderMarkdown("> a long quote that has to wrap around at a narrow width\n>\n> second paragraph", 24))
	want := "│ a long quote that has\n│ to wrap around at a\n│ narrow width\n│\n│ second paragraph"
	if got != want {
		t.Fatalf("quote = %q, want %q", got, want)
	}
	assertMarkdownWidth(t, got, 24)
	assertNoTrailingBlanks(t, got)
}

// A hyphen ends a row only between two alphanumerics, as in the wrapper Codex
// uses (textwrap's HyphenSplitter, whose hyphen_splitter_breaks_at_hyphen test
// is the first case): a flag is never cut into "--" and its name.
func TestRenderMarkdownBreaksAfterAHyphenOnlyBetweenAlphanumerics(t *testing.T) {
	tests := []struct {
		name, source string
		width        int
		want         string
	}{
		{"hyphenated word", "hello-world", 8, "hello-\nworld"},
		{"digits are alphanumerics", "build-2024", 8, "build-\n2024"},
		{"flag stays whole", "ab --result", 10, "ab\n--result"},
		{"short flag stays whole", "run -v please", 8, "run -v\nplease"},
		{"spaced hyphen", "aaa - bbb", 8, "aaa -\nbbb"},
		{"doubled hyphen inside a word", "foo--barbaz", 8, "foo--bar\nbaz"},
		{"flag in a tight list item", "- ab --result", 12, "• ab\n  --result"},
		{"flag in a loose list item", "- ab --result\n\n- x", 12, "• ab\n  --result\n\n• x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := plainMarkdown(RenderMarkdown(tc.source, tc.width)); got != tc.want {
				t.Fatalf("rendered %q at %d = %q, want %q", tc.source, tc.width, got, tc.want)
			}
		})
	}
	// A line that already holds the stand-in character keeps it as written.
	if got := plainMarkdown(RenderMarkdown("a\u2011b --c", 30)); got != "a\u2011b --c" {
		t.Fatalf("text with U+2011 was altered: %q", got)
	}
}

// The three commands below are one per line in the source and stay one per row;
// a command longer than the row wraps at spaces and never leaves a bare "--".
func TestRenderMarkdownKeepsEachCommandLineOnItsOwnRow(t *testing.T) {
	source := "Equivalent commands:\n" +
		"aws codebuild start-build --profile cw3 --project-name one\n" +
		"aws codebuild start-build --profile cw3 --project-name two\n" +
		"aws codebuild start-build --profile cw3 --project-name three"
	if got := plainMarkdown(RenderMarkdown(source, 100)); got != source {
		t.Fatalf("lines were run together:\n%s", got)
	}
	narrow := plainMarkdown(RenderMarkdown(source, 40))
	assertMarkdownWidth(t, narrow, 40)
	starts := 0
	for _, row := range strings.Split(narrow, "\n") {
		if strings.HasPrefix(row, "aws codebuild") {
			starts++
		}
		if row == "--" || strings.HasSuffix(row, " --") {
			t.Fatalf("a flag was cut at its hyphens: %q", row)
		}
	}
	if starts != 3 {
		t.Fatalf("%d rows start a command, want 3:\n%s", starts, narrow)
	}
}

// Code lines wrap at words and continue under the line's own indentation, as
// Codex pre-wraps history lines (subsequent_indent = leading whitespace). The
// fixed-column cut this replaced split "--confirm" into "--con" and "firm".
func TestRenderMarkdownCodeLinesWrapAtWordsUnderTheirOwnIndent(t *testing.T) {
	source := "Run:\n\n```sh\nrun --alpha --beta --gamma\n    indented --one --two --three\n```"
	got := plainMarkdown(RenderMarkdown(source, 22))
	want := "Run:\n\n  run --alpha --beta\n  --gamma\n      indented --one\n      --two --three"
	if got != want {
		t.Fatalf("code rows =\n%s\nwant\n%s", got, want)
	}
	assertMarkdownWidth(t, got, 22)
}

// A token wider than the row is still broken: a row never overflows. An
// indentation wider than half the row is not carried into the continuation.
func TestRenderMarkdownCodeLineLimits(t *testing.T) {
	token := "abcdefghijklmnopqrstuvwxyz0123456789"
	got := plainMarkdown(RenderMarkdown("Run:\n\n```\n"+token+"\n```", 20))
	assertMarkdownWidth(t, got, 20)
	var joined strings.Builder
	for _, row := range strings.Split(got, "\n")[2:] {
		joined.WriteString(strings.TrimPrefix(row, "  "))
	}
	if joined.String() != token {
		t.Fatalf("a broken token lost characters: %q", joined.String())
	}

	blank := strings.Split(plainMarkdown(RenderMarkdown("Run:\n\n```\none\n\ntwo\n```", 20)), "\n")
	if len(blank) != 5 || strings.TrimSpace(blank[3]) != "" || blank[2] != "  one" || blank[4] != "  two" {
		t.Fatalf("a blank line inside code was lost: %q", blank)
	}

	deep := "Run:\n\n```\n" + strings.Repeat(" ", 12) + "one two three four five\n```"
	got = plainMarkdown(RenderMarkdown(deep, 16))
	assertMarkdownWidth(t, got, 16)
	if !strings.Contains(strings.Join(strings.Fields(got), " "), "one two three four five") {
		t.Fatalf("deeply indented code lost words:\n%s", got)
	}
}

// lipgloss pads the shorter lines of a multi-line string to the widest, which
// would underline or strike through blanks after a span that crosses a break.
func TestRenderMarkdownStyledSpansAcrossABreakKeepNoTrailingBlanks(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("**bold\nstill bold** and ~~old\nstill old~~ and [the documentation\nhere](https://example.com)", 80))
	assertNoTrailingBlanks(t, got)
	if !strings.Contains(got, "bold\nstill bold and old\nstill old and the documentation\nhere (https://example.com)") {
		t.Fatalf("spans did not keep their line breaks:\n%s", got)
	}
}

func assertNoTrailingBlanks(t *testing.T, value string) {
	t.Helper()
	for i, line := range strings.Split(value, "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Fatalf("line %d has trailing blanks: %q", i+1, line)
		}
	}
}

func TestRenderMarkdownWidthMatrix(t *testing.T) {
	source := strings.TrimSpace(`
## 验证结果 🚀

- 支持中文、emoji，以及很长的本地路径 ` + "`/workspace/internal/gateway/cli/transcript_renderer.go:213`" + `。
- Keep the [source target](internal/ui/components/markdown.go:31) visible after wrapping.

> Width is measured in terminal cells.
	`)
	for _, width := range []int{40, 80, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			got := plainMarkdown(RenderMarkdown(source, width))
			assertMarkdownWidth(t, got, width)
			for _, want := range []string{"验证结果 🚀", "• 支持中文", "markdown.go:31", "│ Width"} {
				if !strings.Contains(got, want) {
					t.Fatalf("width %d output missing %q:\n%s", width, want, got)
				}
			}
		})
	}
}

func plainMarkdown(value string) string {
	return strings.TrimSpace(ansi.Strip(value))
}

func assertMarkdownWidth(t *testing.T, value string, width int) {
	t.Helper()
	for i, line := range strings.Split(value, "\n") {
		if got := runewidth.StringWidth(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d: %q", i+1, got, width, line)
		}
	}
}
