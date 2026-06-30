package tools

import (
	"strings"
	"testing"
)

func TestUnifiedLineDiffNewFile(t *testing.T) {
	lines, added, removed := unifiedLineDiff("", "a\nb\nc", 3)
	if added != 3 || removed != 0 {
		t.Fatalf("new file: +%d -%d, want +3 -0", added, removed)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "+") {
			t.Fatalf("new-file diff should be all-added, got %q", l)
		}
	}
}

func TestUnifiedLineDiffOverwrite(t *testing.T) {
	old := "keep1\nkeep2\nOLD\nkeep3"
	neu := "keep1\nkeep2\nNEW\nkeep3"
	lines, added, removed := unifiedLineDiff(old, neu, 3)
	if added != 1 || removed != 1 {
		t.Fatalf("+%d -%d, want +1 -1", added, removed)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "-OLD") || !strings.Contains(joined, "+NEW") {
		t.Fatalf("diff missing -OLD/+NEW: %q", joined)
	}
	if !strings.Contains(joined, " keep2") {
		t.Fatalf("diff should include surrounding context: %q", joined)
	}
}

func TestWriteFileResult(t *testing.T) {
	if got := writeFileResult("/x", "same", "same", true); got != "No change to /x" {
		t.Fatalf("identical overwrite: %q", got)
	}
	created := writeFileResult("/x", "", "l1\nl2", false)
	if !strings.HasPrefix(created, "Created /x (+2 -0)") {
		t.Fatalf("created header: %q", created)
	}
	edited := writeFileResult("/x", "a\nb", "a\nc", true)
	if !strings.HasPrefix(edited, "Edited /x (+1 -1)") {
		t.Fatalf("edited header: %q", edited)
	}
}
