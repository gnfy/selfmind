package kernel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitUTF8PrefixKeepTailPreservesChineseRunes(t *testing.T) {
	input := strings.Repeat("当前连接的模型是 SelfMind。", 20)
	prefix, tail := splitUTF8PrefixKeepTail(input, 32)
	if prefix == "" {
		t.Fatal("expected a non-empty prefix")
	}
	if tail == "" {
		t.Fatal("expected a non-empty tail")
	}
	if !utf8.ValidString(prefix) {
		t.Fatalf("prefix is invalid UTF-8: %q", prefix)
	}
	if !utf8.ValidString(tail) {
		t.Fatalf("tail is invalid UTF-8: %q", tail)
	}
	if prefix+tail != input {
		t.Fatalf("split should preserve content")
	}
}
