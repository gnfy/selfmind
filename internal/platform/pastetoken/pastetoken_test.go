package pastetoken

import (
	"strings"
	"testing"
)

// TestFormatProducesExpandableToken is the invariant the two divergent literal
// patterns broke: a token this package builds must survive its own detector and
// must never carry a bracket or a line break, whatever the label contains.
func TestFormatProducesExpandableToken(t *testing.T) {
	labels := []string{
		"main.go.. (80 lines) .. end",
		"## Trycwill.com.. (42 lines) .. Auto or `3600`",
		"[80 lines]",
		"Screenshot [1].png",
		"head\r\nmiddle\rtail",
		"",
	}
	for _, label := range labels {
		for _, kind := range []string{KindPaste, KindImage} {
			token := Format(kind, 0, label)
			inner := strings.TrimSuffix(strings.TrimPrefix(token, "[[ "), " ]]")
			if strings.ContainsAny(inner, "[]") {
				t.Fatalf("token %q (label %q) carries a bracket inside its delimiters", token, label)
			}
			if strings.ContainsAny(token, "\r\n") {
				t.Fatalf("token %q (label %q) spans lines", token, label)
			}
			if !ContainsUnresolved(token) {
				t.Fatalf("detector missed its own token %q", token)
			}
			if got := FindUnresolved("before " + token + " after"); got != token {
				t.Fatalf("FindUnresolved = %q, want %q", got, token)
			}
		}
	}
}

// TestContainsUnresolvedStillCatchesLegacyTokens keeps the guard permissive: an
// older client (or a recalled history entry) can still submit the bracketed
// label form, and the daemon must keep rejecting it.
func TestContainsUnresolvedStillCatchesLegacyTokens(t *testing.T) {
	legacy := []string{
		"Inspect this content: [[ paste:0 main.go.. [80 lines] .. end ]]",
		"[[ paste:12 [1.2K lines] ]]",
		"look at [[ image:0 shot [1].png ]] please",
	}
	for _, text := range legacy {
		if !ContainsUnresolved(text) {
			t.Fatalf("legacy token not detected: %q", text)
		}
	}
}

func TestContainsUnresolvedIgnoresOrdinaryText(t *testing.T) {
	safe := []string{
		"",
		"see docs/STATUS.md [priority] and [[nested]] notes",
		"array[[0]] indexing",
		"paste:0 without delimiters",
		"[[ paste:abc not an index ]]",
	}
	for _, text := range safe {
		if ContainsUnresolved(text) {
			t.Fatalf("false positive on %q", text)
		}
	}
}

// TestFindUnresolvedSeparatesTwoTokensOnOneLine protects the error message: a
// greedy pattern reported the whole span between two tokens as one placeholder.
func TestFindUnresolvedSeparatesTwoTokensOnOneLine(t *testing.T) {
	line := Format(KindPaste, 0, "a") + " and " + Format(KindImage, 1, "b.png")
	if got := FindUnresolved(line); got != Format(KindPaste, 0, "a") {
		t.Fatalf("FindUnresolved = %q, want the first token only", got)
	}
}
