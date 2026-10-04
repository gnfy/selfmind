package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	value := "你可以这样使用AI工具"
	got := TruncateBytes(value, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("TruncateBytes returned invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("TruncateBytes should not introduce replacement runes: %q", got)
	}
}

func TestHeadTailKeepsValidUTF8(t *testing.T) {
	value := strings.Repeat("中文", 100)
	got := HeadTail(value, 17, "\n...\n")
	if !utf8.ValidString(got) {
		t.Fatalf("HeadTail returned invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "\n...\n") {
		t.Fatalf("HeadTail should include marker: %q", got)
	}
}

func TestCleanUTF8RemovesReplacementRune(t *testing.T) {
	got := CleanUTF8("推理���循环")
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("CleanUTF8 should remove replacement runes: %q", got)
	}
	if got != "推理循环" {
		t.Fatalf("CleanUTF8 = %q, want replacement runes removed", got)
	}
}

func TestRuneBoundaryNeverSplitsACharacter(t *testing.T) {
	value := "a中😀b"
	want := []int{0, 1, 1, 1, 4, 4, 4, 4, 8, 9}
	for i := 0; i <= len(value); i++ {
		if got := RuneBoundary(value, i); got != want[i] {
			t.Errorf("RuneBoundary(%q, %d) = %d, want %d", value, i, got, want[i])
		}
	}
	if RuneBoundary(value, 42) != len(value) || RuneBoundary(value, -1) != 0 {
		t.Fatal("an index outside the string must clamp to it")
	}
}
