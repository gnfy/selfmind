// Package pastetoken owns the composer placeholder contract shared by the
// client that PRODUCES a token and the daemon that REJECTS an unexpanded one.
//
// The rules that keep the two sides consistent:
//
//   - Format is the ONLY way to build a token. It sanitizes the label so a
//     token can never contain a bracket or a newline.
//   - ContainsUnresolved recognizes only the current public format. The former
//     `[[ paste:0 ... ]]` spelling is ordinary text and has no compatibility
//     behavior.
//   - Expansion is exact string replacement against the registered token, never
//     a pattern match. A label change can no longer strand a payload.
package pastetoken

import (
	"fmt"
	"regexp"
	"strings"
)

// Kinds of composer placeholders. Both live in the same namespace because the
// daemon's ingress guard treats them identically: the real payload (clipboard
// text, local image path) exists only inside the submitting client.
const (
	KindPaste = "paste"
	KindImage = "image"
)

// tokenRE matches only the canonical composer placeholders. Labels cannot
// contain a closing bracket, so two tokens on one line remain independent.
var tokenRE = regexp.MustCompile(`\[(?:Paste|Image) #[1-9][0-9]* · [^\]\r\n]*\]`)

// labelBreakRE collapses vertical whitespace after brackets have been removed.
var labelBreakRE = regexp.MustCompile(`[\r\n]+`)

// Format renders the display token for a stored payload. The label is a
// human-readable hint only; the payload is recovered from the client's registry
// by exact token match, so sanitizing the label is free.
func Format(kind string, index int, label string) string {
	title := "Paste"
	if kind == KindImage {
		title = "Image"
	}
	return fmt.Sprintf("[%s #%d · %s]", title, index+1, SanitizeLabel(label))
}

// SanitizeLabel strips the characters a token label must never contain and
// collapses the result to a single line. An empty label stays empty.
//
// Invalid UTF-8 is dropped rather than substituted, because the token doubles as
// a registry key: a text widget that coerces a stray byte to U+FFFD would
// otherwise hold a value that no longer equals the stored token.
func SanitizeLabel(label string) string {
	cleaned := strings.NewReplacer("[", "", "]", "").Replace(strings.ToValidUTF8(label, ""))
	cleaned = labelBreakRE.ReplaceAllString(cleaned, " ")
	return strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
}

// ContainsUnresolved reports whether text still carries a placeholder. The
// daemon rejects such a message at ingress and the client refuses to submit it,
// because the payload cannot be recovered once the composer has moved on.
func ContainsUnresolved(text string) bool {
	return tokenRE.MatchString(text)
}

// FindUnresolved returns the first surviving placeholder, for an error message
// that names what failed instead of asking the person to guess.
func FindUnresolved(text string) string {
	return tokenRE.FindString(text)
}
