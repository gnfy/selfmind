package httpapi

import "selfmind/internal/platform/pastetoken"

// The daemon must reject a current-format unexpanded composer placeholder
// ([Paste #N · size] / [Image #N · name]) because the real payload lives only
// in the submitting client and cannot be recovered after submission.
//
// The pattern itself is owned by internal/platform/pastetoken, shared with the
// composer that produces the tokens. Keeping a second literal here is what let
// the two sides disagree. Former `[[...]]` spellings are ordinary text.
func containsUnresolvedPasteToken(content string) bool {
	return pastetoken.ContainsUnresolved(content)
}
