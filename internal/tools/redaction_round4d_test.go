package tools

import (
	"fmt"
	"strings"
	"testing"
)

// Classes 6 and 7 of the documented residual (docs/tool-safety.md), pinned:
// the multi-line literal beyond what is read, and a value field that lies
// further from its name than 8 lines or 512 bytes. A change in either
// direction must update the document.
func TestDocumentedResidualOfLiteralsAndPairs(t *testing.T) {
	var body []string
	for i := 0; i < 60; i++ {
		body = append(body, fmt.Sprintf("r4Block%02dAbCdEf", i))
	}
	closed := "password = \"\"\"\n" + strings.Join(body, "\n") + "\n\"\"\""
	unclosed := "password = \"\"\"\n" + strings.Join(body, "\n")
	// A block closed within 64 KB is masked whole in the result; one that is
	// not closed, only for its first 512 bytes.
	if result := RedactSensitive(closed); strings.Contains(result, "r4Block00") || strings.Contains(result, "r4Block59") {
		t.Errorf("a closed block literal in the result:\n%s", result)
	}
	if result := RedactSensitive(unclosed); strings.Contains(result, "r4Block00") || !strings.Contains(result, "r4Block59") {
		t.Errorf("an unclosed block literal in the result:\n%s", result)
	}
	// A multi-line string of a call is looked at within 512 bytes.
	call := "os.Setenv(\"API_TOKEN\", \"" + strings.Join(body, "\n") + "\")"
	if result := RedactSensitive(call); strings.Contains(result, "r4Block00") || !strings.Contains(result, "r4Block59") {
		t.Errorf("a long multi-line string of a call in the result:\n%s", result)
	}
	// The stream masks the lines within its lookback of 256 bytes after the
	// opening quote, and shows the others.
	if stream := streamRedact(closed); strings.Contains(stream, "r4Block00") || !strings.Contains(stream, "r4Block59") {
		t.Errorf("a long block literal in the stream:\n%s", stream)
	}
	// Within reach, both surfaces mask the whole literal.
	checkBothSurfaces(t, "block", "password = \"\"\"\n"+strings.Join(body[:8], "\n")+"\n\"\"\"", "r4Block00", "r4Block07")

	// A value field more than 8 lines after its name.
	farBelow := "- name: DB_PASSWORD\n" + strings.Repeat("  # comment\n", 9) + "  value: Xk9ab12Cd"
	// A value field further than 512 bytes after its name.
	farBytes := "- name: DB_PASSWORD\n  # " + strings.Repeat("x", 600) + "\n  value: Xk9ab12Cd"
	// A value field more than 8 lines before its name.
	farAbove := "- value: Xk9ab12Cd\n" + strings.Repeat("  # comment\n", 9) + "  name: DB_PASSWORD"
	for name, in := range map[string]string{"below": farBelow, "bytes": farBytes, "above": farAbove} {
		for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
			if name == "above" && surface == "stream" {
				// An unresolved value now fails closed before bounded streaming
				// releases it; only the full result retains this residual.
				if strings.Contains(out, "Xk9ab12Cd") {
					t.Error("the stream released an unresolved value")
				}
				continue
			}
			if !strings.Contains(out, "Xk9ab12Cd") {
				t.Errorf("a value field far %s was paired in the %s (update docs/tool-safety.md if this is a fix): %.80q", name, surface, out)
			}
		}
	}
	// The same fields within reach are paired.
	checkBothSurfaces(t, "near", "- name: DB_PASSWORD\n"+strings.Repeat("  # comment\n", 7)+"  value: Xk9ab12Cd", "Xk9ab12Cd")
	checkBothSurfaces(t, "near", "- value: Xk9ab12Cd\n  name: DB_PASSWORD", "Xk9ab12Cd")
	// In a stream, a `value:` line waits for the lines of its entry: a comment,
	// a blank line or another field between it and its name changes nothing.
	for _, spaced := range []string{
		"- value: Xk9ab12Cd\n  # note\n  name: DB_PASSWORD",
		"- value: Xk9ab12Cd\n\n  name: DB_PASSWORD",
		"- value: Xk9ab12Cd\n  other: 1\n  name: DB_PASSWORD",
		"{\n  \"value\": \"Xk9ab12Cd\",\n  \"name\": \"DB_PASSWORD\"\n}",
		"{\n  \"value\": \"Xk9ab12Cd\",\n  \"other\": 1,\n  \"name\": \"DB_PASSWORD\"\n}",
	} {
		checkBothSurfaces(t, "value above name", spaced, "Xk9ab12Cd")
	}
}
