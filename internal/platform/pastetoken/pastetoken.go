// Package pastetoken owns the composer placeholder contract shared by the
// client that PRODUCES a token and the daemon that REJECTS an unexpanded one.
//
// It exists because the two sides used to carry their own literal pattern in
// two packages. They diverged: the composer's own label embedded "[80 lines]",
// its expansion pattern forbade "]" inside a token, and the daemon's guard
// allowed it. Every large paste therefore failed to expand on the client and
// was then rejected by the daemon with "the pasted content was not expanded" —
// a 100% reproducible dead end for the feature.
//
// The rules that keep the two sides consistent:
//
//   - Format is the ONLY way to build a token. It sanitizes the label so a
//     token can never contain a bracket or a newline.
//   - ContainsUnresolved stays deliberately permissive (it must still catch
//     tokens minted by older clients, including the legacy bracket label), so
//     it is a safety net, never the expansion mechanism.
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

// tokenRE matches any composer placeholder, including the legacy bracketed
// label form. `[^\r\n]*?` is lazy so a line carrying two tokens yields two
// matches instead of one span swallowing the text between them.
var tokenRE = regexp.MustCompile(`\[\[ (?:paste|image):[0-9]+ [^\r\n]*?\]\]`)

// labelUnsafeRE collapses everything that could break the token's own
// delimiters: brackets (which made the old expansion pattern fail) and any
// vertical whitespace (which would split a token across lines).
var labelUnsafeRE = regexp.MustCompile(`[\[\]\r\n]+`)

// Format renders the display token for a stored payload. The label is a
// human-readable hint only; the payload is recovered from the client's registry
// by exact token match, so sanitizing the label is free.
func Format(kind string, index int, label string) string {
	return fmt.Sprintf("[[ %s:%d %s ]]", kind, index, SanitizeLabel(label))
}

// SanitizeLabel strips the characters a token label must never contain and
// collapses the result to a single line. An empty label stays empty: Format
// still produces a well-formed token ("[[ paste:0  ]]").
//
// Invalid UTF-8 is dropped rather than substituted, because the token doubles as
// a registry key: a text widget that coerces a stray byte to U+FFFD would
// otherwise hold a value that no longer equals the stored token.
func SanitizeLabel(label string) string {
	cleaned := labelUnsafeRE.ReplaceAllString(strings.ToValidUTF8(label, ""), " ")
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
