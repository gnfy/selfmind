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

func TestRenderMarkdownTableAdaptsToWidth(t *testing.T) {
	source := strings.TrimSpace(`
| Name | Status |
| --- | --- |
| api | ready |
| background-worker | blocked |
`)

	wide := plainMarkdown(RenderMarkdown(source, 48))
	wantWide := strings.TrimSpace(`
Name              │ Status
──────────────────┼────────
api               │ ready
background-worker │ blocked
`)
	if wide != wantWide {
		t.Fatalf("wide table mismatch\n--- got ---\n%s\n--- want ---\n%s", wide, wantWide)
	}

	narrow := plainMarkdown(RenderMarkdown(source, 20))
	wantNarrow := strings.TrimSpace(`
Name: api
Status: ready

Name:
  background-worker
Status: blocked
`)
	if narrow != wantNarrow {
		t.Fatalf("narrow table mismatch\n--- got ---\n%s\n--- want ---\n%s", narrow, wantNarrow)
	}
	assertMarkdownWidth(t, narrow, 20)
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
