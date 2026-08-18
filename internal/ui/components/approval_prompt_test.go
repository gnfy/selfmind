package components

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestApprovalPromptViewRendersPanel(t *testing.T) {
	p := NewApprovalPrompt("write_file", "/mnt/d/wwwroot/ai/game/ember-citadel-tank-battle.html", "accesses path outside project root")
	view := p.View(80)

	for _, want := range []string{
		"Would you like to make the following edits?",
		"ember-citadel-tank-battle.html",
		"reason: accesses path outside project root",
		"Yes, proceed",
		"No, continue without making edits",
		"esc to cancel",
		"(y)", "(n)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("panel missing %q:\n%s", want, view)
		}
	}
	// The cursor marker sits on the first option by default.
	lines := strings.Split(view, "\n")
	marked := -1
	for i, ln := range lines {
		if strings.Contains(ln, "❯") {
			marked = i
			break
		}
	}
	if marked < 0 || !strings.Contains(lines[marked], "Yes, proceed") {
		t.Fatalf("cursor marker not on the first option:\n%s", view)
	}
}

func TestApprovalPromptViewFitsWidth(t *testing.T) {
	longTarget := "/very/long/path/" + strings.Repeat("segment/", 20) + "file-with-a-remarkably-long-name.html"
	p := NewApprovalPrompt("terminal", longTarget, "invokes dangerous command: "+strings.Repeat("x", 200))
	for _, width := range []int{20, 40, 60, 76, 200} {
		view := p.View(width)
		max := width
		if max > approvalPanelMaxWidth {
			max = approvalPanelMaxWidth
		}
		for _, ln := range strings.Split(view, "\n") {
			plain := stripANSIForTest(ln)
			if w := runewidth.StringWidth(plain); w > max {
				t.Errorf("width=%d: line overflows (%d > %d): %q", width, w, max, plain)
			}
		}
	}
}

func TestApprovalPromptCursorMovesAndClamps(t *testing.T) {
	p := NewApprovalPrompt("write_file", "", "reason")
	if p.Cursor() != 0 {
		t.Fatalf("initial cursor = %d", p.Cursor())
	}
	// Clamp at top.
	if opt := p.HandleKey("up"); opt != nil || p.Cursor() != 0 {
		t.Fatalf("up at top: opt=%v cursor=%d", opt, p.Cursor())
	}
	p.HandleKey("down")
	p.HandleKey("j")
	if p.Cursor() != 1 {
		t.Fatalf("cursor after down,j = %d, want 1", p.Cursor())
	}
	p.HandleKey("k")
	if p.Cursor() != 0 {
		t.Fatalf("cursor after k = %d, want 0", p.Cursor())
	}
	// Clamp at bottom.
	for i := 0; i < 10; i++ {
		p.HandleKey("down")
	}
	if p.Cursor() != len(p.Options())-1 {
		t.Fatalf("cursor did not clamp at bottom: %d", p.Cursor())
	}
}

func TestApprovalPromptEnterSelectsHighlighted(t *testing.T) {
	p := NewApprovalPrompt("write_file", "", "reason")
	p.HandleKey("down")
	opt := p.HandleKey("enter")
	if opt == nil {
		t.Fatal("enter returned no option")
	}
	if opt.Decision != "rejected" || opt.Scope != "" {
		t.Fatalf("enter on second option = %+v, want rejected/once", opt)
	}
}

func TestApprovalPromptShortcutsAnswerDirectly(t *testing.T) {
	cases := []struct {
		key      string
		decision string
		scope    string
	}{
		{"y", "approved", ""},
		{"n", "rejected", ""},
		{"Y", "approved", ""}, // case-insensitive
	}
	for _, tc := range cases {
		p := NewApprovalPrompt("write_file", "", "reason")
		opt := p.HandleKey(tc.key)
		if opt == nil {
			t.Fatalf("key %q returned no option", tc.key)
		}
		if opt.Decision != tc.decision || opt.Scope != tc.scope {
			t.Fatalf("key %q = %+v, want %s/%s", tc.key, opt, tc.decision, tc.scope)
		}
	}
}

// TestApprovalPromptEscAndUnknownKeysIgnored: the passive component delegates
// Esc to the controller, where it becomes an explicit rejection. Stray keys
// never answer locally.
func TestApprovalPromptEscAndUnknownKeysIgnored(t *testing.T) {
	p := NewApprovalPrompt("write_file", "", "reason")
	for _, key := range []string{"esc", "q", "x", " ", "tab", "backspace", "1", "ctrl+d"} {
		if opt := p.HandleKey(key); opt != nil {
			t.Fatalf("key %q answered the prompt: %+v", key, opt)
		}
	}
	if p.Cursor() != 0 {
		t.Fatalf("ignored keys moved the cursor: %d", p.Cursor())
	}
}

func TestTruncateMiddle(t *testing.T) {
	cases := []struct {
		in  string
		max int
	}{
		{"/mnt/d/wwwroot/ai/game/ember-citadel-tank-battle.html", 30},
		{"short", 30},
		{strings.Repeat("宽", 40), 21},
		{"abcdef", 1},
	}
	for _, tc := range cases {
		got := TruncateMiddle(tc.in, tc.max)
		if w := runewidth.StringWidth(got); w > tc.max {
			t.Errorf("TruncateMiddle(%q,%d) width %d exceeds max", tc.in, tc.max, w)
		}
		if runewidth.StringWidth(tc.in) <= tc.max && got != tc.in {
			t.Errorf("TruncateMiddle(%q,%d) modified a fitting string: %q", tc.in, tc.max, got)
		}
	}
	if got := TruncateMiddle("/mnt/d/wwwroot/ai/game/ember-citadel-tank-battle.html", 30); !strings.Contains(got, "…") ||
		!strings.HasPrefix(got, "/mnt/d") || !strings.HasSuffix(got, ".html") {
		t.Errorf("middle-out truncation lost head/tail: %q", got)
	}
}

// stripANSIForTest removes SGR sequences so width assertions measure the
// visible content.
func stripANSIForTest(s string) string {
	var sb strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
