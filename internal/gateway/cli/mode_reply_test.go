package cli

import (
	"strings"
	"testing"
)

// The terminal's /mode is session-local: it pins the mode on this session's
// later messages and never writes the person's saved preference. Queued and
// parked work is narrowed only by that saved preference, so tightening here
// does not reach it, and the reply must not let the person believe it does. It
// names how to narrow that work instead; it does not quietly persist a
// person-level preference from a session-local command.
func TestTerminalModeReplyStatesWhatItDoesNotNarrow(t *testing.T) {
	c := NewController("", "", nil, "default")
	m := c.model
	m.handleMode([]string{"read-only"})
	reply := m.messages[len(m.messages)-1].Content
	for _, want := range []string{
		"read-only",
		"this session",
		"queued or parked work keeps the mode it was admitted with",
		"gateway /mode",
		"/stop",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("the /mode reply must say %q:\n%s", want, reply)
		}
	}
	if m.approvalMode != "read-only" {
		t.Fatalf("the session mode = %q", m.approvalMode)
	}
}
