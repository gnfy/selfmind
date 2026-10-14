package components

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// wrapMarkdownWords wraps text to width cells the way Codex's wrapper does
// (wrapping.rs over textwrap's first-fit layout with its HyphenSplitter), keeping
// the styling escapes of the text:
//
//   - a line breaks at spaces, and after a hyphen only between two alphanumerics,
//     so `--result` is never cut into `--` and `result` and `source-version` may
//     split after `source-`, the hyphen staying with the piece before it;
//   - inside a word it breaks where UAX #14 allows (breakAllowed): between CJK
//     characters and where CJK meets Latin text, with no row starting with
//     closing punctuation or ending with opening punctuation, and after a slash;
//   - a line that holds a URL breaks at spaces only, so the URL stays whole while
//     it fits a row;
//   - a word or piece wider than a row is cut into rows of its width;
//   - spaces at a break are dropped, so no row begins or ends with one.
//
// Newlines in value start new lines; each is wrapped on its own.
func wrapMarkdownWords(value string, width int) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(wrapLineWords(line, max(1, width)), "\n")
	}
	return strings.Join(lines, "\n")
}

// wrapToken is a unit of a line: an escape sequence (no width) or a grapheme.
type wrapToken struct {
	text   string
	width  int
	escape bool
	space  bool
	hyphen bool
	word   bool // the grapheme starts with a letter or a number
	first  rune // its first character
}

func wrapTokens(line string) []wrapToken {
	var tokens []wrapToken
	var state byte
	for len(line) > 0 {
		seq, width, n, next := ansi.DecodeSequence(line, state, nil)
		if n <= 0 {
			break
		}
		state = next
		line = line[n:]
		token := wrapToken{text: seq, width: width}
		if seq != "" && seq[0] == 0x1b {
			token.escape = true
		} else {
			first, _ := utf8.DecodeRuneInString(seq)
			token.first = first
			token.space = unicode.IsSpace(first) && first != '\u00a0'
			token.hyphen = seq == "-"
			token.word = unicode.IsLetter(first) || unicode.IsNumber(first)
		}
		tokens = append(tokens, token)
	}
	return tokens
}

// wrapWord is a run of text between spaces, cut into pieces where it may break,
// and the spaces that preceded it.
type wrapWord struct {
	gap      string
	gapWidth int
	pieces   []wrapPiece
}

type wrapPiece struct {
	text  string
	width int
}

func splitWrapWords(tokens []wrapToken, holdBreaks bool) []wrapWord {
	var words []wrapWord
	var gap strings.Builder
	gapWidth := 0
	var current *wrapWord
	var piece strings.Builder
	pieceWidth := 0
	endPiece := func() {
		if piece.Len() > 0 {
			current.pieces = append(current.pieces, wrapPiece{text: piece.String(), width: pieceWidth})
			piece.Reset()
			pieceWidth = 0
		}
	}
	endWord := func() {
		if current != nil {
			endPiece()
			words = append(words, *current)
			current = nil
		}
	}
	for i, token := range tokens {
		if token.space {
			endWord()
			gap.WriteString(token.text)
			gapWidth += token.width
			continue
		}
		if current == nil {
			current = &wrapWord{gap: gap.String(), gapWidth: gapWidth}
			gap.Reset()
			gapWidth = 0
		}
		piece.WriteString(token.text)
		pieceWidth += token.width
		if !holdBreaks && !token.escape && breakAfter(tokens, i) {
			endPiece()
		}
	}
	endWord()
	return words
}

// breakAfter reports whether the word may break after the token at i: after a
// hyphen between two letters or numbers, or where the line-break rules allow it
// between this character and the next visible one of the same word.
func breakAfter(tokens []wrapToken, i int) bool {
	if tokens[i].hyphen {
		return hyphenBetweenWordCharacters(tokens, i)
	}
	for j := i + 1; j < len(tokens); j++ {
		if !tokens[j].escape {
			return breakAllowed(tokens[i].first, tokens[j].first)
		}
	}
	return false
}

// hyphenBetweenWordCharacters reports whether the hyphen at i has a letter or a
// number on each side, looking past styling escapes.
func hyphenBetweenWordCharacters(tokens []wrapToken, i int) bool {
	before, after := -1, -1
	for j := i - 1; j >= 0; j-- {
		if !tokens[j].escape {
			before = j
			break
		}
	}
	for j := i + 1; j < len(tokens); j++ {
		if !tokens[j].escape {
			after = j
			break
		}
	}
	return before >= 0 && after >= 0 && tokens[before].word && tokens[after].word
}

func wrapLineWords(line string, width int) []string {
	words := splitWrapWords(wrapTokens(line), lineHasURLLikeToken(line))
	var rows []string
	var row strings.Builder
	rowWidth := 0
	endRow := func() {
		rows = append(rows, row.String())
		row.Reset()
		rowWidth = 0
	}
	addPiece := func(piece wrapPiece) {
		if piece.width <= width-rowWidth {
			row.WriteString(piece.text)
			rowWidth += piece.width
			return
		}
		// Wider than a row: cut into rows of the row's width.
		chunks := strings.Split(ansi.Hardwrap(piece.text, width, false), "\n")
		for i, chunk := range chunks {
			if i > 0 {
				endRow()
			}
			row.WriteString(chunk)
			rowWidth = ansi.StringWidth(chunk)
		}
	}
	for _, word := range words {
		for i, piece := range word.pieces {
			gap, gapWidth := "", 0
			if i == 0 {
				gap, gapWidth = word.gap, word.gapWidth
			}
			if rowWidth > 0 && rowWidth+gapWidth+piece.width > width {
				endRow()
			}
			if rowWidth > 0 {
				row.WriteString(gap)
				rowWidth += gapWidth
			}
			addPiece(piece)
		}
	}
	if row.Len() > 0 {
		endRow()
	}
	return rows
}
