package pastetoken

import (
	"strings"
	"testing"
)

// TestFormatProducesExpandableToken pins the invariant that a token this
// package builds survives its own detector and never spans lines.
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

func TestFormatUsesHumanOneBasedOrdinal(t *testing.T) {
	if got := Format(KindPaste, 0, "80 lines"); got != "[Paste #1 · 80 lines]" {
		t.Fatalf("Format paste = %q", got)
	}
	if got := Format(KindImage, 1, "screenshot.png"); got != "[Image #2 · screenshot.png]" {
		t.Fatalf("Format image = %q", got)
	}
}

// Former double-bracket placeholders are ordinary text. There is deliberately
// no compatibility detector or expansion path for them.
func TestContainsUnresolvedIgnoresLegacyTokens(t *testing.T) {
	legacy := []string{
		"Inspect this content: [[ paste:0 main.go.. [80 lines] .. end ]]",
		"[[ paste:12 [1.2K lines] ]]",
		"look at [[ image:0 shot [1].png ]] please",
	}
	for _, text := range legacy {
		if ContainsUnresolved(text) {
			t.Fatalf("legacy token unexpectedly detected: %q", text)
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
		"[Paste #0 · invalid ordinal]",
		"[paste #1 · wrong case]",
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
