package tools

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maskSpan is a half-open byte range of the original text to mask. Every
// detector reports spans on the text it was given; masking applies their
// union once, so one match can never hide part of another from view.
type maskSpan struct{ start, end int }

// mergeSpans sorts spans and joins those that overlap or touch.
func mergeSpans(spans []maskSpan) []maskSpan {
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := make([]maskSpan, 0, len(spans))
	for _, span := range spans {
		if span.end <= span.start {
			continue
		}
		if n := len(merged); n > 0 && span.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, span.end)
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

// applyMasks replaces the union of spans with one mask per merged range.
func applyMasks(text string, spans []maskSpan) string {
	merged := mergeSpans(spans)
	if len(merged) == 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	pos := 0
	for _, span := range merged {
		out.WriteString(text[pos:span.start])
		out.WriteString("[REDACTED]")
		pos = span.end
	}
	out.WriteString(text[pos:])
	return out.String()
}

// covered reports whether offset lies inside one of the merged spans.
func covered(merged []maskSpan, offset int) bool {
	i := sort.Search(len(merged), func(i int) bool { return merged[i].end > offset })
	return i < len(merged) && merged[i].start <= offset
}

// textView is a text with some bytes left out or decoded, keeping for every
// byte it holds the original range that byte stands for. Detection runs on
// the view and its spans are mapped back, so a mask always covers whole
// original units: an escape sequence is never split.
type textView struct {
	text  string
	start []int32
	end   []int32
}

// original maps a span of the view back to the text the view came from.
func (v *textView) original(span maskSpan) maskSpan {
	return maskSpan{int(v.start[span.start]), int(v.end[span.end-1])}
}

type viewBuilder struct {
	text       strings.Builder
	start, end []int32
}

func (b *viewBuilder) keep(original string, at, n int) {
	b.text.WriteString(original[at : at+n])
	for i := at; i < at+n; i++ {
		b.start = append(b.start, int32(i))
		b.end = append(b.end, int32(i+1))
	}
}

func (b *viewBuilder) decoded(char byte, at, n int) {
	b.text.WriteByte(char)
	b.start = append(b.start, int32(at))
	b.end = append(b.end, int32(at+n))
}

func (b *viewBuilder) view() *textView {
	return &textView{text: b.text.String(), start: b.start, end: b.end}
}

// detectionView spells text the way a reader sees it: it leaves out terminal
// color and control sequences (jq -C, grep --color), invisible format
// characters (zero-width spaces and joiners, the soft hyphen, bidirectional
// controls), and decodes a JSON \uXXXX escape that stands for a letter,
// digit, '_' or '-'. It returns nil when the text holds none of them, the
// common case, so ordinary text costs one scan.
func detectionView(text string) *textView {
	if strings.IndexByte(text, 0x1b) < 0 && !hasFormatCharacter(text) && !hasIdentifierEscape(text) {
		return nil
	}
	b := &viewBuilder{start: make([]int32, 0, len(text)), end: make([]int32, 0, len(text))}
	for i := 0; i < len(text); {
		if n := terminalSequenceAt(text, i); n > 0 {
			i += n
			continue
		}
		if char, ok := identifierEscapeAt(text, i); ok {
			b.decoded(char, i, 6)
			i += 6
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r >= 0x80 && unicode.Is(unicode.Cf, r) {
			i += size
			continue
		}
		b.keep(text, i, size)
		i += size
	}
	return b.view()
}

// terminalSequenceAt is the length of the ANSI escape sequence at i: a
// control sequence (ESC [ parameters final), an operating system command
// (ESC ] ... BEL or ESC \), or another two-byte escape. It is 0 when none
// starts there.
func terminalSequenceAt(text string, i int) int {
	if text[i] != 0x1b || i+1 >= len(text) {
		return 0
	}
	switch text[i+1] {
	case '[':
		for j := i + 2; j < len(text); j++ {
			if c := text[j]; c >= 0x40 && c <= 0x7e {
				return j + 1 - i
			} else if c < 0x20 || c > 0x7e {
				return 0
			}
		}
		return 0
	case ']':
		for j := i + 2; j < len(text); j++ {
			switch {
			case text[j] == 0x07:
				return j + 1 - i
			case text[j] == 0x1b && j+1 < len(text) && text[j+1] == '\\':
				return j + 2 - i
			case text[j] == '\n':
				return 0
			}
		}
		return 0
	}
	if c := text[i+1]; c >= 0x40 && c <= 0x5f {
		return 2
	}
	return 0
}

func hasFormatCharacter(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < utf8.RuneSelf {
			continue
		}
		for _, r := range text[i:] {
			if r >= 0x80 && unicode.Is(unicode.Cf, r) {
				return true
			}
		}
		return false
	}
	return false
}

func hasIdentifierEscape(text string) bool {
	for at := 0; ; {
		i := strings.Index(text[at:], `\u`)
		if i < 0 {
			return false
		}
		if _, ok := identifierEscapeAt(text, at+i); ok {
			return true
		}
		at += i + 2
	}
}

// identifierEscapeAt decodes a \uXXXX escape at i that is a real escape (its
// backslash is not itself escaped) and stands for an identifier byte.
func identifierEscapeAt(text string, i int) (byte, bool) {
	if i+6 > len(text) || text[i] != '\\' || text[i+1] != 'u' || text[i+2] != '0' || text[i+3] != '0' {
		return 0, false
	}
	value, ok := hexByte(text[i+4], text[i+5])
	if !ok || !isIdentifierByte(value) {
		return 0, false
	}
	backslashes := 0
	for j := i; j >= 0 && text[j] == '\\'; j-- {
		backslashes++
	}
	return value, backslashes%2 == 1
}

func hexByte(high, low byte) (byte, bool) {
	h, okHigh := hexDigit(high)
	l, okLow := hexDigit(low)
	return h<<4 | l, okHigh && okLow
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// whitespaceFreeView leaves out every whitespace byte, so a JSON value
// matches whatever its layout.
func whitespaceFreeView(text string) *textView {
	b := &viewBuilder{start: make([]int32, 0, len(text)), end: make([]int32, 0, len(text))}
	for i := 0; i < len(text); i++ {
		if isJSONSpace(text[i]) {
			continue
		}
		b.keep(text, i, 1)
	}
	return b.view()
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func withoutWhitespace(text string) string {
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && isJSONSpace(byte(r)) {
			return -1
		}
		return r
	}, text)
}

// asciiLower lowercases ASCII letters only, so byte offsets stay the same.
func asciiLower(text string) string {
	upper := strings.IndexFunc(text, func(r rune) bool { return r >= 'A' && r <= 'Z' })
	if upper < 0 {
		return text
	}
	out := []byte(text)
	for i := upper; i < len(out); i++ {
		if c := out[i]; c >= 'A' && c <= 'Z' {
			out[i] = c + 'a' - 'A'
		}
	}
	return string(out)
}
