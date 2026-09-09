package cli

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// An approval stops the run until it is answered, and the panel that says so is
// only visible to someone already looking at this terminal. Nothing reached a
// person working in another window.
func TestTerminalAttentionSequencePerTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  terminalNotifyEnv
		want string
	}{
		{
			name: "a terminal that renders OSC 9 gets the message",
			env:  terminalNotifyEnv{TermProgram: "ghostty"},
			want: "\x1b]9;approval needed\x07",
		},
		{
			name: "iTerm2 announces itself through LC_TERMINAL",
			env:  terminalNotifyEnv{LCTerminal: "iTerm2"},
			want: "\x1b]9;approval needed\x07",
		},
		{
			name: "kitty announces itself through TERM",
			env:  terminalNotifyEnv{Term: "xterm-kitty"},
			want: "\x1b]9;approval needed\x07",
		},
		{
			// OSC 9 in a terminal that does not implement it prints as text, so
			// everything unrecognized gets the bell instead.
			name: "an unrecognized terminal gets the bell, never a stray payload",
			env:  terminalNotifyEnv{Term: "xterm-256color", TermProgram: "Apple_Terminal"},
			want: "\x07",
		},
		{
			// tmux forwards an escape sequence to the outer terminal only inside
			// a DCS wrapper, with every ESC in the payload doubled.
			name: "inside tmux the sequence is wrapped for passthrough",
			env:  terminalNotifyEnv{TermProgram: "ghostty", InTmux: true},
			want: "\x1bPtmux;\x1b\x1b]9;approval needed\x07\x1b\\",
		},
		{
			name: "an explicit opt-out sends nothing at all",
			env:  terminalNotifyEnv{TermProgram: "ghostty", Disabled: true},
			want: "",
		},
	} {
		got := terminalAttentionSequence("approval needed", tc.env)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A notification body is a display surface of its own, outside the redaction the
// transcript applies. An unterminated escape inside it would run on into the
// rest of the frame.
func TestTerminalAttentionStripsControlBytesFromTheMessage(t *testing.T) {
	got := terminalAttentionSequence("approval\x1b[31m needed\x07", terminalNotifyEnv{TermProgram: "kitty"})
	if strings.Count(got, "\x1b") != 1 || strings.Count(got, "\x07") != 1 {
		t.Fatalf("control bytes survived into the payload: %q", got)
	}
	if !strings.Contains(got, "approval[31m needed") {
		t.Fatalf("sequence = %q", got)
	}
}

// The signal stays quiet while the person is demonstrably watching — the panel
// is already in front of them. It must NOT stay quiet when the terminal has
// never reported focus: many terminals and multiplexers never do, and treating
// silence as "focused" would remove the signal exactly where it is needed.
func TestTerminalAttentionRespectsFocusOnlyWhenReported(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("SELF_TUI_ATTENTION", "")
	t.Setenv("TMUX", "")

	silent := NewController("", "", nil, "").model
	silent.terminalFocused = false
	if silent.notifyTerminalAttention("approval needed") == nil {
		t.Error("a terminal that never reports focus must still be alerted")
	}

	watching := NewController("", "", nil, "").model
	watching.Update(tea.FocusMsg{})
	if watching.notifyTerminalAttention("approval needed") != nil {
		t.Error("a watched terminal must not be belled at")
	}

	away := NewController("", "", nil, "").model
	away.Update(tea.BlurMsg{})
	if away.notifyTerminalAttention("approval needed") == nil {
		t.Error("an unfocused terminal must be alerted")
	}
}

// Arming a prompt is called from several places, some with no command of their
// own to return, so the signal has to reach the runtime through the deferral
// the committed transcript lines already use.
func TestArmingAHumanWaitQueuesTheTerminalSignal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("SELF_TUI_ATTENTION", "")
	t.Setenv("TMUX", "")

	model := NewController("", "", nil, "").model
	model.Update(tea.BlurMsg{})

	model.armApprovalPrompt(sampleApproval("apr_1"))
	if len(model.pendingCmds) != 1 {
		t.Fatalf("an armed approval queued %d signals", len(model.pendingCmds))
	}
	// flushPendingPrintln is what actually hands them to the runtime.
	if model.flushPendingPrintln(nil) == nil {
		t.Fatal("the queued signal never reached the runtime")
	}
	if len(model.pendingCmds) != 0 {
		t.Fatal("the queue was not drained")
	}
}

// The notification body is derived from the kind, never supplied by the caller.
// A desktop notification is a display surface outside the redaction the
// transcript applies, and the previous shape left that rule living only in a
// comment at each call site — the next call site would have missed it.
func TestAttentionSubjectAdmitsOnlyAnIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   humanWaitKind
		detail string
		want   string
	}{
		{"a tool name is useful and safe", humanWaitApproval, "terminal", "SelfMind needs your approval: terminal"},
		{"a dotted role name passes", humanWaitApproval, "watch_external", "SelfMind needs your approval: watch_external"},
		// A command, a path, or a sentence is DROPPED, not truncated: half a
		// command in a notification body is worse than no detail.
		{"a command is dropped", humanWaitApproval, "gcloud builds approve 49d9", "SelfMind needs your approval"},
		{"a path is dropped", humanWaitApproval, "/Users/cwill/.config/gcloud", "SelfMind needs your approval"},
		{"a model-authored sentence is dropped", humanWaitApproval, "should I delete the bucket?", "SelfMind needs your approval"},
		{"an over-long token is dropped", humanWaitApproval, strings.Repeat("a", 41), "SelfMind needs your approval"},
		// A clarification's question is model-authored, so the kind carries no
		// detail at all.
		{"a clarification names no detail", humanWaitClarification, "should I delete the bucket?", "SelfMind is waiting on your answer"},
		{"an unknown kind still says something true", humanWaitKind("handoff"), "x", "SelfMind is waiting on you"},
	} {
		if got := attentionSubject(tc.kind, tc.detail); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
