package cli

import "strings"

// splitStableMarkdownPrefix keeps an unfinished Markdown block literal until a
// blank line closes it. The rule is deliberately conservative: a paragraph,
// list, table, or fenced block may gain new semantics while tokens are still
// arriving, whereas a blank-line-terminated block outside a fence cannot be
// reinterpreted by a later block. Final cells still use the full GFM renderer.
func splitStableMarkdownPrefix(source string) (stable, tail string) {
	if source == "" {
		return "", ""
	}
	stableEnd := 0
	offset := 0
	inFence := false
	fenceRune := byte(0)
	fenceLen := 0
	for _, part := range strings.SplitAfter(source, "\n") {
		offset += len(part)
		line := strings.TrimSpace(strings.TrimSuffix(part, "\n"))
		if marker, count := markdownFenceMarker(line); count >= 3 {
			switch {
			case !inFence:
				inFence = true
				fenceRune, fenceLen = marker, count
			case marker == fenceRune && count >= fenceLen:
				inFence = false
			}
		}
		if !inFence && line == "" && strings.HasSuffix(part, "\n") {
			stableEnd = offset
		}
	}
	return source[:stableEnd], source[stableEnd:]
}

func markdownFenceMarker(line string) (byte, int) {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	marker := line[0]
	count := 0
	for count < len(line) && line[count] == marker {
		count++
	}
	return marker, count
}
