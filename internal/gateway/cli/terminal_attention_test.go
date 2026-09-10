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
		kind   attentionKind
		detail string
		want   string
	}{
		{"a tool name is useful and safe", attentionApproval, "terminal", "SelfMind needs your approval: terminal"},
		{"a dotted role name passes", attentionApproval, "watch_external", "SelfMind needs your approval: watch_external"},
		// A command, a path, or a sentence is DROPPED, not truncated: half a
		// command in a notification body is worse than no detail.
		{"a command is dropped", attentionApproval, "gcloud builds approve 49d9", "SelfMind needs your approval"},
		{"a path is dropped", attentionApproval, "/Users/cwill/.config/gcloud", "SelfMind needs your approval"},
		{"a model-authored sentence is dropped", attentionApproval, "should I delete the bucket?", "SelfMind needs your approval"},
		{"an over-long token is dropped", attentionApproval, strings.Repeat("a", 41), "SelfMind needs your approval"},
		// A clarification's question is model-authored, so the kind carries no
		// detail at all.
		{"a clarification names no detail", attentionClarification, "should I delete the bucket?", "SelfMind is waiting on your answer"},
		{"an unknown kind still says something true", attentionKind("handoff"), "x", "SelfMind is waiting on you"},
		// The three surfaces added after the first two, same rule: detail is an
		// identifier or nothing.
		{"a parked approval names its tool", attentionParkedApproval, "terminal", "SelfMind still needs your approval: terminal"},
		{"a parked approval drops a command", attentionParkedApproval, "rm -rf build", "SelfMind still needs your approval"},
		{"finished background work names its watcher", attentionBackgroundDone, "watch_9f2c", "SelfMind finished background work: watch_9f2c"},
		{"finished background work drops a summary", attentionBackgroundDone, "deployed 3 services to prod", "SelfMind finished background work"},
		{"a background failure carries no detail", attentionBackgroundFailed, "watch_9f2c", "SelfMind background work failed"},
	} {
		if got := attentionSubject(tc.kind, tc.detail); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The three surfaces that had events but no signal. Each is asserted through
// the real Update/handler path, not by calling signalAttention directly: what
// matters is that the site fires, not that the function works.
func TestParkedApprovalBackgroundOutcomeAndFailureSignalTheTerminal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("SELF_TUI_ATTENTION", "")
	t.Setenv("TMUX", "")
	unfocused := func() *uiModel {
		m := NewController("", "", nil, "").model
		m.Update(tea.BlurMsg{})
		return m
	}
	drain := func(m *uiModel) int {
		n := len(m.pendingCmds)
		m.pendingCmds = nil
		return n
	}

	// A parked approval: nothing is visibly running any more, so it is the
	// approval most easily left unanswered.
	parked := unfocused()
	parked.armApprovalPrompt(sampleApproval("apr_parked"))
	drain(parked) // the arm itself signalled; the park is a second moment
	parked.markApprovalParked("apr_parked")
	if got := drain(parked); got != 1 {
		t.Errorf("parking an approval queued %d signals, want 1", got)
	}

	// A background run reaching its outcome: the person delegated it so they
	// could look away. startNoticeFinalizer registers a watcher's finalizer run
	// the same way the daemon feed does, so the finish lands on the
	// background branch rather than the foreground one.
	background := unfocused()
	startNoticeFinalizer(background)
	drain(background)
	background.updateInner(MsgDaemonRunFinished{RunID: "run_finalizer", Status: "done", Summary: "release recorded",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_finalizer", Cursor: 20}})
	if got := drain(background); got != 1 {
		t.Errorf("a finished background run queued %d signals, want 1", got)
	}
	// A foreground finish is the person's own turn ending in front of them.
	foreground := unfocused()
	foreground.updateInner(MsgDaemonRunFinished{RunID: "run_fg", Status: "done",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_fg", Cursor: 21}})
	if got := drain(foreground); got != 0 {
		t.Errorf("a foreground finish queued %d signals, want 0", got)
	}

	// A background failure is worth hearing about; a routine success is not.
	// updateInner is the handler before Update flushes the queue into the
	// returned command; the returned command is not a usable probe because the
	// handler's own addNotice produces a Println command either way.
	failed := unfocused()
	failed.updateInner(MsgBackgroundNotice{Content: "watcher could not reach the API", Success: false})
	if got := drain(failed); got != 1 {
		t.Errorf("a background failure queued %d signals, want 1", got)
	}
	succeeded := unfocused()
	succeeded.updateInner(MsgBackgroundNotice{Content: "watcher registered", Success: true})
	if got := drain(succeeded); got != 0 {
		t.Errorf("a routine background success queued %d signals, want 0", got)
	}
}

// A watched terminal is never belled at, for the new surfaces exactly as for
// the first two: the outcome is already on screen in front of them.
func TestNewAttentionSurfacesStayQuietWhileWatched(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("SELF_TUI_ATTENTION", "")
	t.Setenv("TMUX", "")
	m := NewController("", "", nil, "").model
	m.Update(tea.FocusMsg{})

	m.armApprovalPrompt(sampleApproval("apr_1"))
	m.markApprovalParked("apr_1")
	m.updateInner(MsgBackgroundNotice{Content: "failed", Success: false})
	if len(m.pendingCmds) != 0 {
		t.Fatalf("a watched terminal queued %d signals", len(m.pendingCmds))
	}
}
