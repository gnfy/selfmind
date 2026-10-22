package components

import (
	"strings"
	"testing"
)

// Codex's text_contains_url_like_matches_expected_tokens and
// text_contains_url_like_rejects_non_urls, with the rules each token exercises.
func TestIsURLLikeToken(t *testing.T) {
	positives := []string{
		"https://example.com/a/b",
		"ftp://host/path",
		"www.example.com/path?x=1",
		"example.test/path#frag",
		"localhost:3000/api",
		"127.0.0.1:8080/health",
		"(https://example.com/wrapped-in-parens)",
		"myapp://open/some/path",
		"x+y.z-w://rest",
		"https://example.com/x,",
		"www.example.com",
		"WWW.Example.COM",
		"example.com?x=1",
		"example.com:8080/x",
		"localhost:65535/x",
		"LOCALHOST/x",
		"10.0.0.1/x",
		"255.255.255.255/x",
		"a--b.com/x",
		"sub.domain.example.org/x",
		"[https://example.com/x]",
		"{https://example.com/x}",
		"'https://example.com/x'",
		"\"https://example.com/x\"",
		"<https://example.com/x>.",
		"https://example.com/x!",
		"https://example.com/x;",
		"https://example.com/x:",
		"https://example.com/x.",
		"https://example.com/x)",
		"(https://example.com/x",
		"myapp:///x",
		"foo-://",
		"foo://%zz",
		"ws://host/x",
		"wss://host/x",
		"ftps://host/x",
		"HTTPS://EXAMPLE.COM/X",
		"example.com#frag",
		// A scheme the URL parser rejects (bad escape) is judged by its characters.
		"a+b://%zz", "a-b://%zz", "a.b://%zz", "a1://%zz", "x+y.z-w1://%zz",
	}
	for _, token := range positives {
		if !isURLLikeToken(token) {
			t.Errorf("%q should look like a URL", token)
		}
	}
	negatives := []string{
		"src/main.rs",
		"foo/bar",
		"key:value",
		"just-some-text-with-dashes",
		"hello.world",
		"example.com",
		"localhost:99999/path",
		"localhost:65536/x",
		"localhost:123456/x",
		"example.com:abc/path",
		"https://",
		"https:///path",
		"://rest",
		"1abc://rest",
		"256.0.0.1/x",
		"1.2.3/x",
		"1.2.3.4.5/x",
		"a_b.com/x",
		"-a.com/x",
		"a-.com/x",
		"example.c/x",
		"example.c0m/x",
		"/path/only",
		"?query",
		"...",
		"",
		"[::1]:8080/x",
		"http:///x",
		"ftp:///x",
		"ftps:///x",
		"ws:///x",
		"wss:///x",
		"1foo://%zz",
		"fo o://%zz",
		"://%zz",
		"a..com/x",
		"www.example.com:/x",
		"localhost:/x",
		"1x://%zz", "-x://%zz", "x_y://%zz", "x y://%zz", "x%://%zz",
	}
	for _, token := range negatives {
		if isURLLikeToken(token) {
			t.Errorf("%q should not look like a URL", token)
		}
	}
	// Whatever surrounds a URL in prose is trimmed from both ends before it is judged:
	// a bare host without a trailer only counts as www.example.com itself.
	for _, c := range "()[]{}<>,.;:!'\"" {
		for _, token := range []string{string(c) + "www.example.com", "www.example.com" + string(c), string(c) + "www.example.com" + string(c)} {
			if !isURLLikeToken(token) {
				t.Errorf("%q should look like a URL", token)
			}
		}
	}
	if !isURLLikeToken(strings.Repeat("a", 63) + ".com/x") {
		t.Error("a label of 63 characters is valid")
	}
	if isURLLikeToken(strings.Repeat("a", 64) + ".com/x") {
		t.Error("a label of 64 characters is not valid")
	}
	if !isURLLikeToken("example." + strings.Repeat("a", 63) + "/x") {
		t.Error("a top-level label of 63 letters is valid")
	}
	if isURLLikeToken("example." + strings.Repeat("a", 64) + "/x") {
		t.Error("a top-level label of 64 letters is not valid")
	}
}

// Codex's line_contains_url_like_checks_across_spans: styling does not hide a URL.
func TestLineHasURLLikeToken(t *testing.T) {
	if !lineHasURLLikeToken("see \x1b[36mhttps://example.com/a/very/long/path\x1b[0m for details") {
		t.Fatal("a styled URL was not found")
	}
	if lineHasURLLikeToken("src/main.rs and key:value") {
		t.Fatal("a line of paths and words has no URL")
	}
	if lineHasURLLikeToken("") {
		t.Fatal("an empty line has no URL")
	}
	if !lineHasURLLikeToken("a\thttps://example.com/x") || !lineHasURLLikeToken("a\rhttps://example.com/x") {
		t.Fatal("tabs and carriage returns separate tokens")
	}
	if !lineHasURLLikeToken("a\fhttps://example.com/x") || !lineHasURLLikeToken("a\nhttps://example.com/x") {
		t.Fatal("form feeds and newlines separate tokens")
	}
	if lineHasURLLikeToken("a\u00a0https://example.com/x") != false {
		t.Fatal("only ASCII whitespace separates tokens")
	}
}

// A line that holds a URL is wrapped at spaces only, so the URL is not cut at its
// hyphens while it fits a row; the same words with no URL still split after an
// alphanumeric hyphen.
func TestRenderMarkdownKeepsAURLWholeAtItsHyphens(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("go to https://x.io/a-b-c-d now please", 24))
	if want := "go to\nhttps://x.io/a-b-c-d now\nplease"; got != want {
		t.Fatalf("with a URL = %q, want %q", got, want)
	}
	got = plainMarkdown(RenderMarkdown("go to xx.io-a-b-c-d now please", 24))
	if want := "go to xx.io-a-b-c-d now\nplease"; got != want {
		t.Fatalf("without a URL = %q, want %q", got, want)
	}
	got = plainMarkdown(RenderMarkdown("go to my-long-word-here-b-c-d-e now please", 18))
	if want := "go to my-long-\nword-here-b-c-d-e\nnow please"; got != want {
		t.Fatalf("hyphenated words still split = %q, want %q", got, want)
	}
}

// Codex's adaptive_wrap_line_mixed_line_keeps_regular_words_intact.
func TestRenderMarkdownWrapsProseAroundAURL(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("see https://example.com/path and keep strikethrough intact while wrapping prose", 36))
	if want := "see https://example.com/path and\nkeep strikethrough intact while\nwrapping prose"; got != want {
		t.Fatalf("mixed line = %q, want %q", got, want)
	}
}

// On a line with a URL the prose is wrapped at spaces too: a word wider than the
// row is cut at the row's width, not at its hyphen.
func TestRenderMarkdownDoesNotSplitProseAtAHyphenNextToAURL(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("see a.io/x source-version ok", 10))
	if want := "see a.io/x\nsource-ver\nsion ok"; got != want {
		t.Fatalf("mixed line = %q, want %q", got, want)
	}
	got = plainMarkdown(RenderMarkdown("see source-version ok", 10))
	if want := "see\nsource-\nversion ok"; got != want {
		t.Fatalf("without a URL = %q, want %q", got, want)
	}
}

// A hard line break starts a new line: the decision is made per line, so a URL on
// one line does not stop the hyphens of the next from splitting.
func TestRenderMarkdownDecidesAboutURLsPerLine(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("x.io/a\nsee source-version ok", 11))
	if want := "x.io/a\nsee source-\nversion ok"; got != want {
		t.Fatalf("a line without a URL should split at its hyphen, got %q want %q", got, want)
	}
	got = plainMarkdown(RenderMarkdown("x.io/a see source-version ok", 11))
	if want := "x.io/a see\nsource-vers\nion ok"; got != want {
		t.Fatalf("a line with a URL should not, got %q want %q", got, want)
	}
}
