package cli

// Terminal-local attention signal for work that has parked on the person.
//
// An approval or a clarification stops the run until it is answered, and the
// panel that says so is only visible to someone already looking at this
// terminal. Nothing reached a person working in another window, so a parked run
// could sit unanswered for as long as it took them to look back.
//
// This is the terminal's own channel, separate from the `/notify` endpoint
// preference (which routes detached CLI-origin pushes to IM): it needs no
// configuration, no bound account, and no network.

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// terminalNotifyEnv is the terminal identity the sequence choice depends on,
// read from the environment so the decision is testable without a terminal.
type terminalNotifyEnv struct {
	TermProgram string
	Term        string
	LCTerminal  string
	InTmux      bool
	Disabled    bool
}

func currentTerminalNotifyEnv() terminalNotifyEnv {
	return terminalNotifyEnv{
		TermProgram: os.Getenv("TERM_PROGRAM"),
		Term:        os.Getenv("TERM"),
		LCTerminal:  os.Getenv("LC_TERMINAL"),
		InTmux:      strings.TrimSpace(os.Getenv("TMUX")) != "",
		Disabled:    strings.EqualFold(strings.TrimSpace(os.Getenv("SELF_TUI_ATTENTION")), "off"),
	}
}

// supportsDesktopNotification reports whether this terminal turns OSC 9 into a
// desktop notification. Emitting it anywhere else risks the payload being
// printed as text, so everything unrecognized gets the bell instead.
func (e terminalNotifyEnv) supportsDesktopNotification() bool {
	program := strings.ToLower(strings.TrimSpace(e.TermProgram))
	term := strings.ToLower(strings.TrimSpace(e.Term))
	lc := strings.ToLower(strings.TrimSpace(e.LCTerminal))
	switch {
	case program == "ghostty", program == "iterm.app", program == "kitty",
		program == "warpterminal", program == "wezterm":
		return true
	case lc == "iterm2":
		return true
	case strings.Contains(term, "kitty"), strings.Contains(term, "ghostty"):
		return true
	}
	return false
}

// terminalAttentionSequence builds the bytes to write, or "" when this terminal
// gets nothing. Both forms are zero-width: they occupy no cells and move no
// cursor, so interleaving them with the renderer's output cannot corrupt a
// frame.
func terminalAttentionSequence(message string, env terminalNotifyEnv) string {
	if env.Disabled {
		return ""
	}
	if !env.supportsDesktopNotification() {
		return "\x07"
	}
	// A notification body is a display surface of its own, outside the redaction
	// the transcript applies, so callers pass a short fixed subject. Strip any
	// control byte anyway: an unterminated escape inside the payload would run
	// on into the rest of the frame.
	message = sanitizeAttentionMessage(message)
	if message == "" {
		return "\x07"
	}
	if env.InTmux {
		// tmux forwards an escape sequence to the outer terminal only inside a
		// DCS wrapper, and every ESC in the wrapped payload must be doubled.
		// Only the OSC introducer needs it here: sanitizeAttentionMessage has
		// already removed every control byte from the message, so the body
		// cannot contain one.
		return "\x1bPtmux;\x1b\x1b]9;" + message + "\x07\x1b\\"
	}
	return "\x1b]9;" + message + "\x07"
}

func sanitizeAttentionMessage(message string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, message)
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > 120 {
		cleaned = cleaned[:120]
	}
	return cleaned
}

// humanWaitKind names a reason a run has stopped and cannot continue until the
// person answers. Every such surface signals the terminal, and this is the list
// of them: adding one is a new constant, a subject line, and one call.
type humanWaitKind string

const (
	humanWaitApproval      humanWaitKind = "approval"
	humanWaitClarification humanWaitKind = "clarification"
)

// attentionSubject is the notification body for one kind.
//
// The body is derived from the kind rather than supplied by the caller, and
// `detail` is admitted only when it has the shape of a daemon-issued
// identifier. A desktop notification is a display surface outside the
// redaction the transcript applies, so a command, a path, or a model-authored
// question must never travel in it — and a rule that lives only in a comment
// at each call site is a rule the next call site will miss.
func attentionSubject(kind humanWaitKind, detail string) string {
	switch kind {
	case humanWaitApproval:
		if name := shortIdentifier(detail); name != "" {
			return "SelfMind needs your approval: " + name
		}
		return "SelfMind needs your approval"
	case humanWaitClarification:
		return "SelfMind is waiting on your answer"
	default:
		return "SelfMind is waiting on you"
	}
}

// shortIdentifier passes through a tool or control name and nothing else. A
// command, a path, or a sentence is dropped rather than truncated: a half
// command in a notification body is worse than no detail at all.
func shortIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 40 || strings.ContainsAny(value, " \t\n/\\") {
		return ""
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return ""
		}
	}
	return value
}

// signalHumanWait alerts this terminal that a run has parked on the person, and
// schedules it for after Update returns: arming a prompt is called from several
// places, some with no command of their own, so the signal rides the same
// deferral the committed transcript lines use.
//
// This is the one entry point. A new human-wait surface calls it with its kind;
// it does not build a message, choose a sequence, or decide about focus.
func (m *uiModel) signalHumanWait(kind humanWaitKind, detail string) {
	if cmd := m.notifyTerminalAttention(attentionSubject(kind, detail)); cmd != nil {
		m.pendingCmds = append(m.pendingCmds, cmd)
	}
}

// notifyTerminalAttention returns a command that alerts this terminal, or nil
// when nothing should be sent.
//
// It stays silent only while the person is demonstrably watching: the panel is
// already in front of them and a bell would be noise. Anything else alerts,
// including a terminal that never reports focus — treating silence about focus
// as "focused" would remove the signal exactly where it is needed most.
func (m *uiModel) notifyTerminalAttention(message string) tea.Cmd {
	if m == nil || m.terminalFocused {
		return nil
	}
	sequence := terminalAttentionSequence(message, currentTerminalNotifyEnv())
	if sequence == "" {
		return nil
	}
	// Written from a command, not from Update: the renderer owns the terminal on
	// that goroutine, and the codebase already keeps writes off it for Println.
	return func() tea.Msg {
		_, _ = os.Stdout.WriteString(sequence)
		return nil
	}
}
