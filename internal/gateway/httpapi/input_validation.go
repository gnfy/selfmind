package httpapi

import "regexp"

// unresolvedPasteTokenRE mirrors the composer's display tokens — large-paste
// ([[ paste:N … ]]) and image-attachment ([[ image:N … ]]). The daemon must
// reject an unexpanded token because the real payload (clipboard text, image
// path) lives only in the submitting client and cannot be recovered after
// submission.
var unresolvedPasteTokenRE = regexp.MustCompile(`\[\[ (?:paste|image):[0-9]+ [^\r\n]*\]\]`)

func containsUnresolvedPasteToken(content string) bool {
	return unresolvedPasteTokenRE.MatchString(content)
}
