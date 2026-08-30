package httpapi

import "testing"

// TestContainsUnresolvedPasteToken covers both composer display tokens: a
// large-paste token and an image-attachment token must be rejected unexpanded
// (the payload lives only in the submitting client), while ordinary text —
// including "/"-leading file paths — passes.
func TestContainsUnresolvedPasteToken(t *testing.T) {
	rejected := []string{
		"Inspect this: [Paste #1 · 80 lines]",
		"看一下 [Image #1 · selfmind-paste-1.png] 这张图",
	}
	for _, in := range rejected {
		if !containsUnresolvedPasteToken(in) {
			t.Errorf("containsUnresolvedPasteToken(%q) = false, want true", in)
		}
	}
	accepted := []string{
		"/tmp/selfmind-paste-1.png 帮我看一下这张图",
		"plain text",
		"[[ paste:x no-digit ]]",
		"[[ paste:0 legacy token is ordinary text ]]",
	}
	for _, in := range accepted {
		if containsUnresolvedPasteToken(in) {
			t.Errorf("containsUnresolvedPasteToken(%q) = true, want false", in)
		}
	}
}
