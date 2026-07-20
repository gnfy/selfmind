package httpapi

import "regexp"

// unresolvedPasteTokenRE mirrors the composer's large-paste display token.
// The daemon must reject this token because the real clipboard payload lives
// only in the submitting client and cannot be recovered after submission.
var unresolvedPasteTokenRE = regexp.MustCompile(`\[\[ paste:[0-9]+ [^\r\n]*\]\]`)

func containsUnresolvedPasteToken(content string) bool {
	return unresolvedPasteTokenRE.MatchString(content)
}
