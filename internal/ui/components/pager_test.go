package components

import (
	"strings"
	"testing"
)

func pagerFooter(p *Pager) string {
	lines := strings.Split(stripANSIForTest(p.View()), "\n")
	return lines[len(lines)-1]
}

func longPage(width int) string { return strings.Repeat("one more line\n", 100) }

// Without a hint the footer is what every pager has always said; a page that
// answers to more keys than q/Esc names them there, where scrolling cannot hide
// them.
func TestPagerFooterShowsTheDefaultOrTheKeysOfThePage(t *testing.T) {
	p := NewPager(nil, 80, 10, longPage)
	if got := strings.TrimRight(pagerFooter(p), " "); got != " line 1  q/Esc close - up/down scroll" {
		t.Fatalf("default footer = %q", got)
	}
	p.SetHint("c select · Esc/b back · q close")
	for range 25 {
		p.viewport.ScrollDown(1)
	}
	got := pagerFooter(p)
	if !strings.Contains(got, "c select · Esc/b back · q close - up/down scroll") || !strings.Contains(got, "line 26") {
		t.Fatalf("footer after scrolling = %q", got)
	}
	p.Resize(60, 12)
	if !strings.Contains(pagerFooter(p), "c select · Esc/b back · q close") {
		t.Fatalf("hint lost on resize: %q", pagerFooter(p))
	}
}

// The viewport is sized for one footer row; a hint longer than the terminal must
// be cut, never wrapped into a second row that pushes the page out of the frame.
func TestPagerFooterStaysOneRowWhateverTheHintLength(t *testing.T) {
	p := NewPager(nil, 30, 10, longPage)
	p.SetHint("c select for your next message · Esc/b back · r refresh · q close")
	view := stripANSIForTest(p.View())
	if rows := len(strings.Split(view, "\n")); rows != 10 {
		t.Fatalf("pager view is %d rows, want 10:\n%s", rows, view)
	}
	if !strings.HasPrefix(pagerFooter(p), " line 1  c select") {
		t.Fatalf("the keys must lead the footer: %q", pagerFooter(p))
	}
}
