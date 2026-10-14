package components

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The pair rules of UAX #14 that matter for CJK text and paths (breakAllowed).
func TestBreakAllowed(t *testing.T) {
	allowed := [][2]rune{
		{'你', '好'}, // between ideographs
		{'定', 'R'}, // where CJK meets Latin
		{'W', '好'}, // and the other way round
		{'，', '世'}, // after closing punctuation
		{'。', 'R'}, // also before Latin text
		{'好', '（'}, // before opening punctuation
		{'a', '（'}, // a wide opening bracket may follow a letter
		{'好', 'a'}, //
		{'好', '1'}, // and a number
		{'/', 'b'}, // after a slash
		{'/', '（'}, //
		{'好', '⺀'}, {'⺀', 'a'},
	}
	for _, pair := range allowed {
		if !breakAllowed(pair[0], pair[1]) {
			t.Errorf("a break between %q and %q should be allowed", pair[0], pair[1])
		}
	}
	forbidden := [][2]rune{
		{'a', 'b'}, {'1', '2'}, {'a', '1'}, {'1', 'a'}, // one script stays whole
		{'a', '('}, {')', 'a'}, // narrow brackets do not split a Latin word
		{'/', '1'}, {'/', '/'}, {'a', '/'}, {'好', '/'}, // a number follows a slash; nothing breaks before one
		{'好', '-'}, {'-', '好'}, {'-', 'a'}, {'a', '-'}, // the hyphen rule's
		{'说', '“'}, {'“', '好'}, {'说', '"'}, {'"', '好'}, {'说', '\''}, {'\'', '好'}, // quotation marks
		{'好', '”'}, {'”', '好'}, {'好', '‘'}, {'’', '好'}, {'好', '«'}, {'»', '好'}, {'好', '‹'}, {'›', '好'},
		{'（', '世'}, {'(', '世'}, {'「', '世'}, // after opening punctuation
		{'⹿', 'a'}, // below the CJK blocks: still one script
	}
	for _, pair := range forbidden {
		if breakAllowed(pair[0], pair[1]) {
			t.Errorf("a break between %q and %q should not be allowed", pair[0], pair[1])
		}
	}
	// Nothing breaks before closing punctuation, exclamation marks, non-starters,
	// an infix separator or a slash.
	for _, c := range ")]}）］｝、。，．」』】》〉〕〗〙〛｡､｣!?！？：；・ー々〜゛゜ぁぃぅぇぉっゃゅょゎゕゖァィゥェォッャュョヮヵヶゝゞヽヾ,.:;/" {
		if breakAllowed('好', c) {
			t.Errorf("a break before %q should not be allowed", c)
		}
		if !breakAllowed(c, '好') {
			t.Errorf("a break after %q should be allowed", c)
		}
	}
	// Nothing breaks after opening punctuation.
	for _, c := range "([{（［｛「『【《〈〔〖〘〚｢" {
		if breakAllowed(c, '好') {
			t.Errorf("a break after %q should not be allowed", c)
		}
		if !breakAllowed('好', c) {
			t.Errorf("a break before %q should be allowed", c)
		}
	}
}

func TestWrapMarkdownWordsBreaksCJKTextWithoutSpaces(t *testing.T) {
	tests := []struct {
		name, text string
		width      int
		want       string
	}{
		{"a long run breaks between ideographs", "你好世界你好", 6, "你好世\n界你好"},
		{"no row starts with a comma", "你好，世界", 4, "你\n好，\n世界"},
		{"no row ends with an opening bracket", "你好（世界）", 6, "你好\n（世\n界）"},
		{"Latin words stay whole inside CJK text", "判定REVIEW通过", 8, "判定\nREVIEW通\n过"},
		{"a list joined by ideographic commas wraps after a comma", "identity、project、release、duplicate", 16, "identity、\nproject、\nrelease、\nduplicate"},
		{"quotation marks keep their neighbours", "他说“好”吗", 8, "他\n说“好”吗"},
		{"a fullwidth colon does not start a row", "构建侧完成：生产侧未生效", 12, "构建侧完成：\n生产侧未生效"},
		{"a path wraps at its slashes", "apps/pps-laravel/overlays/prod/kustomization.yaml", 20, "apps/pps-laravel/\noverlays/prod/\nkustomization.yaml"},
		{"a number after a slash stays with it", "ab 2026/05/25 cd", 7, "ab\n2026/05\n/25 cd"},
		{"a URL line breaks at spaces only", "a.io/x 你好，世界", 4, "a.io\n/x\n你好\n，世\n界"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapMarkdownWords(tc.text, tc.width); got != tc.want {
				t.Fatalf("wrap(%q, %d) =\n%s\nwant\n%s", tc.text, tc.width, got, tc.want)
			}
		})
	}
}

// Whatever the width, no row of Chinese prose begins with closing punctuation or
// ends with opening punctuation, and no row is wider than the width. A piece wider
// than the row (the widest here is 11 cells) is cut by width, as Codex's wrapper
// cuts it, so the widths start above it.
func TestWrapMarkdownWordsKeepsPunctuationInPlace(t *testing.T) {
	text := "判定 REVIEW（cw3 / 463701203291 / us-east-1，工单 RUQX-1273 首次发布）：identity、project、release、" +
		"duplicate、in_flight、work_order 全 PASS，只有 tag_commit WARN。三个 tag 都正好是各仓库生产分支 master 的 HEAD；" +
		"WARN 只是因为 preflight 拿仓库默认分支 dev 做了对比！“三条判据各需一条可复核证据”来收口。"
	closing := "，。：；！？、）】」》”’."
	opening := "（【「《“‘"
	for width := 12; width <= 70; width++ {
		for _, row := range strings.Split(wrapMarkdownWords(text, width), "\n") {
			if row == "" {
				t.Fatalf("width %d: an empty row", width)
			}
			runes := []rune(row)
			if strings.ContainsRune(closing, runes[0]) {
				t.Errorf("width %d: row starts with closing punctuation: %q", width, row)
			}
			if strings.ContainsRune(opening, runes[len(runes)-1]) {
				t.Errorf("width %d: row ends with opening punctuation: %q", width, row)
			}
			if ansi.StringWidth(row) > width {
				t.Errorf("width %d: row is %d cells: %q", width, ansi.StringWidth(row), row)
			}
		}
	}
}

// A path in a table cell wraps at its slashes too.
func TestRenderMarkdownTableCellPathWrapsAtSlashes(t *testing.T) {
	got := renderPlain("| File | Note |\n|---|---|\n| apps/pps-laravel/overlays/prod/kustomization.yaml | ok |\n", 36)
	want := " File                          Note\n" +
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━  ━━━━━━\n" +
		" apps/pps-laravel/overlays/    ok\n" +
		" prod/kustomization.yaml"
	if got != want {
		t.Fatalf("path in a cell =\n%s\nwant\n%s", got, want)
	}
	assertMarkdownWidth(t, got, 36)
}
