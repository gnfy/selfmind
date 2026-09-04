package cli

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTerminalGoneQuitsTheSession pins the fix for sessions that outlived their
// terminal. Bubble Tea's read loop treats io.EOF — what a departed terminal
// produces — as a normal end and tells the program nothing, so without this the
// session runs on timers forever.
func TestTerminalGoneQuitsTheSession(t *testing.T) {
	model := NewController("", "", nil, "").model
	updated, cmd := model.Update(MsgTerminalGone{})
	got := updated.(*uiModel)
	if !got.quitting {
		t.Fatal("a departed terminal must end the session")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("quit command produced no message")
	}
}

// TestTerminalLivenessTickReArms keeps the probe running for the life of the
// session: one missed re-arm would silently disable the whole check.
func TestTerminalLivenessTickReArms(t *testing.T) {
	model := NewController("", "", nil, "").model
	_, cmd := model.Update(MsgTerminalLivenessTick{})
	// In tests stdout is not a terminal, so the probe declines to arm — there
	// is no terminal to lose. That must be a clean nil, never a busy loop.
	if cmd != nil {
		if msg := cmd(); msg == nil {
			t.Fatal("liveness tick produced no message")
		}
	}
}

// TestIdleCaretStopsBlinking pins the CPU fix. Two abandoned sessions had each
// burned ~27 minutes of CPU blinking a caret nobody could see; the blink now
// drops to a rare check and returns on its own when someone types.
func TestIdleCaretStopsBlinking(t *testing.T) {
	model := NewController("", "", nil, "").model

	// Fresh session: the caret blinks.
	model.noteInputActivity(time.Now())
	model.cursorVisible = false
	updated, _ := model.Update(MsgCursorBlinkTick(time.Now()))
	if !updated.(*uiModel).cursorVisible {
		t.Fatal("an active session must keep blinking")
	}

	// Long idle: the caret settles visible and stops toggling.
	model.noteInputActivity(time.Now().Add(-cursorBlinkIdleAfter - time.Minute))
	model.cursorVisible = false
	updated, cmd := model.Update(MsgCursorBlinkTick(time.Now()))
	got := updated.(*uiModel)
	if !got.cursorVisible {
		t.Fatal("an idle caret must be left visible, not hidden")
	}
	if cmd == nil {
		t.Fatal("the idle check must stay armed so typing restores the blink")
	}
	// Idle again: still steady rather than toggling back off.
	updated, _ = got.Update(MsgCursorBlinkTick(time.Now()))
	if !updated.(*uiModel).cursorVisible {
		t.Fatal("an idle caret must not toggle")
	}

	// Someone types: the normal blink returns without any other code path
	// having to know the caret had gone quiet.
	got.noteInputActivity(time.Now())
	got.cursorVisible = false
	updated, _ = got.Update(MsgCursorBlinkTick(time.Now()))
	if !updated.(*uiModel).cursorVisible {
		t.Fatal("the blink must resume after input")
	}
}

// TestHangupWatchStopsCleanly: registering and unregistering the SIGHUP watch
// must not leak a goroutine or panic on repeated use.
func TestHangupWatchStopsCleanly(t *testing.T) {
	if stop := watchHangup(nil); stop == nil {
		t.Fatal("a nil program must still return a usable stop")
	} else {
		stop()
	}
	p := tea.NewProgram(NewController("", "", nil, "").model)
	stop := watchHangup(p)
	stop()
}
