package components

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

func TestRenderMarkdownCoreBlocksGolden(t *testing.T) {
	source := strings.TrimSpace(`
# Result

Paragraph with **bold**, *italic*, ~~old~~, ` + "`code`" + `, and [docs](https://example.com/docs).

> A concise note.

- First item has enough words to wrap with a hanging indent.
  - Nested item

1. Ordered item

` + "```go" + `
fmt.Println("ok")
` + "```" + `
`)

	got := plainMarkdown(RenderMarkdown(source, 44))
	want := strings.TrimSpace(`
Result

Paragraph with bold, italic, old, code, and
docs (https://example.com/docs).

│ A concise note.

• First item has enough words to wrap with a
  hanging indent.
  • Nested item

1. Ordered item

  fmt.Println("ok")
`)
	if got != want {
		t.Fatalf("rendered markdown mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	assertMarkdownWidth(t, got, 44)
	for _, marker := range []string{"# Result", "**bold**", "~~old~~", "```"} {
		if strings.Contains(got, marker) {
			t.Fatalf("rendered markdown leaked source marker %q: %q", marker, got)
		}
	}
}

func TestRenderMarkdownTableAdaptsToWidth(t *testing.T) {
	source := strings.TrimSpace(`
| Name | Status |
| --- | --- |
| api | ready |
| background-worker | blocked |
`)

	wide := plainMarkdown(RenderMarkdown(source, 48))
	wantWide := strings.TrimSpace(`
Name              │ Status
──────────────────┼────────
api               │ ready
background-worker │ blocked
`)
	if wide != wantWide {
		t.Fatalf("wide table mismatch\n--- got ---\n%s\n--- want ---\n%s", wide, wantWide)
	}

	narrow := plainMarkdown(RenderMarkdown(source, 20))
	wantNarrow := strings.TrimSpace(`
Name: api
Status: ready

Name:
  background-worker
Status: blocked
`)
	if narrow != wantNarrow {
		t.Fatalf("narrow table mismatch\n--- got ---\n%s\n--- want ---\n%s", narrow, wantNarrow)
	}
	assertMarkdownWidth(t, narrow, 20)
}

func TestRenderMarkdownKeepsListContinuationIndented(t *testing.T) {
	got := plainMarkdown(RenderMarkdown("- Alpha beta gamma delta epsilon", 18))
	want := "• Alpha beta gamma\n  delta epsilon"
	if got != want {
		t.Fatalf("wrapped list = %q, want %q", got, want)
	}
	assertMarkdownWidth(t, got, 18)
}

func TestRenderMarkdownWidthMatrix(t *testing.T) {
	source := strings.TrimSpace(`
## 验证结果 🚀

- 支持中文、emoji，以及很长的本地路径 ` + "`/workspace/internal/gateway/cli/transcript_renderer.go:213`" + `。
- Keep the [source target](internal/ui/components/markdown.go:31) visible after wrapping.

> Width is measured in terminal cells.
	`)
	for _, width := range []int{40, 80, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			got := plainMarkdown(RenderMarkdown(source, width))
			assertMarkdownWidth(t, got, width)
			for _, want := range []string{"验证结果 🚀", "• 支持中文", "markdown.go:31", "│ Width"} {
				if !strings.Contains(got, want) {
					t.Fatalf("width %d output missing %q:\n%s", width, want, got)
				}
			}
		})
	}
}

func plainMarkdown(value string) string {
	return strings.TrimSpace(ansi.Strip(value))
}

func assertMarkdownWidth(t *testing.T, value string, width int) {
	t.Helper()
	for i, line := range strings.Split(value, "\n") {
		if got := runewidth.StringWidth(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d: %q", i+1, got, width, line)
		}
	}
}
