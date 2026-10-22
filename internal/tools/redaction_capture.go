package tools

import (
	"sort"
	"strings"
)

// capturedLine locates one line of a command's captured output, which merges
// its two streams in the order their lines arrived, and says which wrote it.
type capturedLine struct {
	stderr     bool
	start, end int
}

// maskInterleavedStreams masks what a stream printed across lines when the
// other stream's lines came between them in the merged capture, where it is
// not contiguous: a registered secret, a name and its value on separate
// lines, an AWS key id and its secret, the body of a private key, the strings
// of a credential container. Each stream is read on its own, as the live
// stream reads it, with every detector, and what it finds is masked line by
// line where it lies; whatever the merged text still shows to a detector is
// left to the result middleware, which masks it whole.
func maskInterleavedStreams(output string, lines []capturedLine) string {
	var seen [2]bool
	for _, line := range lines {
		seen[0] = seen[0] || !line.stderr
		seen[1] = seen[1] || line.stderr
	}
	if !seen[0] || !seen[1] {
		return output
	}
	r := currentRedactor()
	// What the merged text shows to a detector is left to the result
	// middleware, which also keeps a JSON document JSON; only what the merge
	// hides is masked here.
	merged, _ := r.spans(output)
	seenInMerged := mergeSpans(merged)
	var masks []maskSpan
	for _, stderr := range []bool{false, true} {
		var text strings.Builder
		var index, offset []int
		for i, line := range lines {
			if line.stderr != stderr {
				continue
			}
			if len(index) > 0 {
				text.WriteByte('\n')
			}
			offset = append(offset, text.Len())
			text.WriteString(output[line.start:line.end])
			index = append(index, i)
		}
		spans, _ := r.spans(text.String())
		for _, span := range mergeSpans(spans) {
			first := sort.Search(len(offset), func(k int) bool { return offset[k] > span.start }) - 1
			last := sort.Search(len(offset), func(k int) bool { return offset[k] >= span.end }) - 1
			if first < 0 || last < first {
				continue
			}
			for k := first; k <= last; k++ {
				line := lines[index[k]]
				from, to := max(span.start, offset[k]), min(span.end, offset[k]+line.end-line.start)
				if from >= to {
					continue
				}
				piece := maskSpan{line.start + from - offset[k], line.start + to - offset[k]}
				if !covered(seenInMerged, piece.start) || !covered(seenInMerged, piece.end-1) {
					masks = append(masks, piece)
				}
			}
		}
	}
	return applyMasks(output, masks)
}
