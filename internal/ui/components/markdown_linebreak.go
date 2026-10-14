package components

import (
	"strings"
	"unicode"
)

// Where text may break inside a word. Codex's wrapper takes its break
// opportunities from UAX #14 (textwrap over unicode-linebreak); this is the part
// of it that matters for CJK text, which has no spaces to break at and which a
// fixed column would cut anywhere, leaving a row that starts with a comma:
//
//   - between two ideographs, and where CJK text meets Latin text, a break is
//     allowed;
//   - never before closing punctuation, exclamation marks, non-starters such as
//     ：； and small kana, an infix separator or a slash, and never after opening
//     punctuation (LB13, LB14, LB21);
//   - never next to a quotation mark (LB19);
//   - letters and numbers of one script stay whole, and a hyphen is the hyphen
//     rule's (textwrap suppresses UAX #14 breaks after it);
//   - after a slash a break is allowed unless a number follows (LB25), which is how
//     a long path wraps at its slashes.
const (
	noBreakBeforeRunes = ")]}）］｝、。，．」』】》〉〕〗〙〛｡､｣!?！？：；・ー々〜゛゜ぁぃぅぇぉっゃゅょゎゕゖァィゥェォッャュョヮヵヶゝゞヽヾ,.:;/"
	noBreakAfterRunes  = "([{（［｛「『【《〈〔〖〘〚｢"
	quoteRunes         = "\"'“”‘’«»‹›"
)

func breakAllowed(a, b rune) bool {
	switch {
	case a == '-' || b == '-':
		return false
	case strings.ContainsRune(quoteRunes, a) || strings.ContainsRune(quoteRunes, b):
		return false
	case strings.ContainsRune(noBreakBeforeRunes, b) || strings.ContainsRune(noBreakAfterRunes, a):
		return false
	case a == '/':
		return !unicode.IsDigit(b)
	}
	return cjkLike(a) || cjkLike(b)
}

// cjkLike covers the CJK blocks, kana, fullwidth forms and what lies above them.
func cjkLike(r rune) bool { return r >= 0x2e80 }
