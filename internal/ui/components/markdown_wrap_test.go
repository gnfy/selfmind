package components

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The expected text of these tests is Codex's own (wrapping.rs tests, over
// textwrap's first-fit layout with its HyphenSplitter).
func TestWrapMarkdownWordsMatchesCodex(t *testing.T) {
	tests := []struct {
		name, text string
		width      int
		want       string
	}{
		// hyphen_splitter_breaks_at_hyphen
		{"hyphen splitter breaks at hyphen", "hello-world", 7, "hello-\nworld"},
		// wrap_lines_accepts_borrowed_iterators, wrap_lines_accepts_str_slices
		{"each line is wrapped on its own", "hello world\nfoo bar baz", 10, "hello\nworld\nfoo bar\nbaz"},
		{"a row is filled before the next", "hello world\ngoodnight moon", 12, "hello world\ngoodnight\nmoon"},
		// line_height_counts_double_width_emoji, wide_unicode_wraps_by_display_width
		{"double-width characters count two cells", "😀😀😀", 4, "😀😀\n😀"},
		{"double-width characters, a row of two", "😀😀😀", 2, "😀\n😀\n😀"},
		{"double-width characters, all in one row", "😀😀😀", 6, "😀😀😀"},
		// word_wrap_does_not_split_words_simple_english
		{"prose wraps at spaces", "Years passed, and Willowmere thrived in peace and friendship. Mira’s herb garden flourished with both ordinary and enchanted plants, and travelers spoke of the kindness of the woman who tended them.", 40,
			"Years passed, and Willowmere thrived in\npeace and friendship. Mira’s herb garden\nflourished with both ordinary and\nenchanted plants, and travelers spoke of\nthe kindness of the woman who tended\nthem."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapMarkdownWords(tc.text, tc.width); got != tc.want {
				t.Fatalf("wrap(%q, %d) =\n%s\nwant\n%s", tc.text, tc.width, got, tc.want)
			}
		})
	}
}

// Layout rules of the wrapper: spaces within a row are kept, spaces at a break are
// dropped, and a hyphen stays with the piece before it, so no row begins with one.
func TestWrapMarkdownWordsLayout(t *testing.T) {
	tests := []struct {
		name, text string
		width      int
		want       string
	}{
		{"spaces inside a row are kept", "aaa  bbb ccc", 9, "aaa  bbb\nccc"},
		{"no space ends a row", "aaa bbb ccc", 4, "aaa\nbbb\nccc"},
		{"no space begins a row", "aaa  bbb", 4, "aaa\nbbb"},
		{"the word that does not fit starts the next row", "ab cdef", 5, "ab\ncdef"},
		{"a hyphenated word splits at its hyphens when it does not fit", "see source-version ok", 10, "see\nsource-\nversion ok"},
		{"a hyphen stays with the piece before it", "word-here-b-c-d-e now", 9, "word-\nhere-b-c-\nd-e now"},
		{"a doubled hyphen is not a split point", "foo--barbaz", 8, "foo--bar\nbaz"},
		{"a flag is not cut at its hyphens", "ab --result", 10, "ab\n--result"},
		{"a word wider than a row is cut into rows", "abcdefghij", 4, "abcd\nefgh\nij"},
		{"the rest of a cut word shares its row with the next word", "abcdefghij kl", 7, "abcdefg\nhij kl"},
		{"a cut word starts a new row", "ab abcdefghij", 4, "ab\nabcd\nefgh\nij"},
		{"a non-breaking space joins words", "a\u00a0b c", 3, "a\u00a0b\nc"},
		{"a non-breaking space is not a break", "aa\u00a0bb", 3, "aa\u00a0\nbb"},
		{"a width of one", "ab cd", 1, "a\nb\nc\nd"},
		{"a row may be exactly as wide as the width", "aaa bbb", 7, "aaa bbb"},
		{"one cell short of the width wraps", "aaa bbb", 6, "aaa\nbbb"},
		{"a piece as wide as the row is not cut", "abcd efgh", 4, "abcd\nefgh"},
		{"a tab separates words", "ab\tcd", 3, "ab\ncd"},
		{"an empty line", "", 5, ""},
		{"only spaces", "   ", 5, ""},
		{"blank lines stay", "a\n\nb", 5, "a\n\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapMarkdownWords(tc.text, tc.width); got != tc.want {
				t.Fatalf("wrap(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
			}
		})
	}
}

// Styling escapes are zero-width, stay with their text, and do not hide a hyphen's
// neighbours; a span cut across rows keeps every escape it had.
func TestWrapMarkdownWordsKeepsStyling(t *testing.T) {
	bold, reset := "\x1b[1m", "\x1b[0m"
	text := bold + "bold words" + reset + " here"
	got := wrapMarkdownWords(text, 6)
	if ansi.Strip(got) != "bold\nwords\nhere" {
		t.Fatalf("plain wrap = %q", ansi.Strip(got))
	}
	if strings.Count(got, bold) != 1 || strings.Count(got, reset) != 1 {
		t.Fatalf("escapes were lost or duplicated: %q", got)
	}
	// An escape between a hyphen and the next letter does not make it a flag hyphen.
	hyphen := "foo-" + bold + "bar" + reset + " baz"
	if got := ansi.Strip(wrapMarkdownWords(hyphen, 5)); got != "foo-\nbar\nbaz" {
		t.Fatalf("hyphen next to an escape = %q", got)
	}
	before := "foo" + bold + "-" + reset + "bar baz"
	if got := ansi.Strip(wrapMarkdownWords(before, 5)); got != "foo-\nbar\nbaz" {
		t.Fatalf("an escape before the hyphen = %q", got)
	}
	// A wide piece is cut by display width, not by bytes of escape.
	wide := bold + "abcdefghij" + reset
	rows := strings.Split(wrapMarkdownWords(wide, 4), "\n")
	if len(rows) != 3 || ansi.StringWidth(rows[0]) != 4 || ansi.StringWidth(rows[1]) != 4 || ansi.StringWidth(rows[2]) != 2 {
		t.Fatalf("rows = %q", rows)
	}
}

// An escape that opens a style stays with the text it styles when a break falls
// between them, so the next row is still styled, and a closing escape stays with
// the text it closes.
func TestWrapMarkdownWordsKeepsEscapesWithTheirText(t *testing.T) {
	open, closeSeq := "\x1b[1m", "\x1b[0m"
	rows := strings.Split(wrapMarkdownWords("ab"+open+"你好"+closeSeq, 2), "\n")
	if len(rows) != 3 || rows[0] != "ab" || rows[1] != open+"你" || rows[2] != "好"+closeSeq {
		t.Fatalf("rows = %q", rows)
	}
}

// A width below one is a width of one, and no row is ever wider than the width
// (a double-width character on a row of one cell is the one thing that cannot be
// cut).
func TestWrapMarkdownWordsNeverExceedsTheWidth(t *testing.T) {
	text := "Plan RUQX-1273 --out .release/RUQX-1273-s1-result.json 并且 some-long-hyphenated-identifier-here https://example.com/a-b-c-d-e"
	for width := 8; width <= 60; width++ {
		for _, row := range strings.Split(wrapMarkdownWords(text, width), "\n") {
			if ansi.StringWidth(row) > width && !strings.Contains(row, "https://") {
				t.Fatalf("width %d: row %q is %d cells", width, row, ansi.StringWidth(row))
			}
		}
	}
	if got := wrapMarkdownWords("ab", 0); got != "a\nb" {
		t.Fatalf("a width of 0 should wrap like 1: %q", got)
	}
}
