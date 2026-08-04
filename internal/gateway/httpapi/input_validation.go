package httpapi

import "selfmind/internal/platform/pastetoken"

// The daemon must reject an unexpanded composer placeholder ([[ paste:N … ]] /
// [[ image:N … ]]) because the real payload (clipboard text, image path) lives
// only in the submitting client and cannot be recovered after submission.
//
// The pattern itself is owned by internal/platform/pastetoken, shared with the
// composer that produces the tokens. Keeping a second literal here is what let
// the two sides disagree: the composer could not expand a label containing "]",
// while this guard rejected exactly that token.
func containsUnresolvedPasteToken(content string) bool {
	return pastetoken.ContainsUnresolved(content)
}
