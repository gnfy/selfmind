package cli

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// The TUI used to outlive its terminal. Two orphans were found running for
// 2 days 21 hours after their windows were gone, each having burned ~27 minutes
// of CPU blinking a cursor nobody could see.
//
// The cause is a gap in Bubble Tea rather than a missed signal: its read loop
// treats io.EOF — exactly what a departed terminal produces — as a normal end
// and returns without telling the program anything (tty.go readLoop). From then
// on nothing can arrive except timers, so a program with any repeating tick
// runs forever against a terminal that is gone.
//
// SIGHUP is not a sufficient answer either. Bubble Tea registers SIGINT,
// SIGTERM, SIGWINCH and SIGCONT but not SIGHUP, so Go's default disposition
// (terminate) applies — meaning that if SIGHUP had been delivered those
// processes would already have died. They did not, so no hangup was ever
// delivered, and no signal handler could have saved them.
//
// What IS observable is the terminal itself. Measured on macOS with a real pty:
// after the master closes, IsTerminal still reports true and tcgetattr still
// succeeds — both useless — while a write returns EIO. A ZERO-BYTE write is
// enough: it is a no-op while the terminal lives and returns EIO once it is
// gone, so the check costs nothing and displays nothing.

// terminalLivenessInterval bounds how long an abandoned session can linger.
// The probe is a zero-byte write, so this can be frequent without cost; 30s
// keeps a departed terminal from holding a process for more than half a minute.
const terminalLivenessInterval = 30 * time.Second

// MsgTerminalGone is delivered when the controlling terminal has gone away.
type MsgTerminalGone struct{}

// terminalLivenessTick schedules the next liveness probe. It returns nil when
// stdout is not a terminal (piped output, tests, CI), where there is no
// terminal to lose and the probe would mean nothing.
func terminalLivenessTick() tea.Cmd {
	if !isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		return nil
	}
	return tea.Tick(terminalLivenessInterval, func(time.Time) tea.Msg {
		if terminalIsGone() {
			return MsgTerminalGone{}
		}
		return MsgTerminalLivenessTick{}
	})
}

// MsgTerminalLivenessTick re-arms the probe after a live check.
type MsgTerminalLivenessTick struct{}

// terminalIsGone reports whether the controlling terminal has been closed.
// The zero-byte write writes nothing and returns EIO once the far end is gone.
func terminalIsGone() bool {
	_, err := os.Stdout.Write(nil)
	return err != nil
}

// watchHangup quits the program on SIGHUP.
//
// Bubble Tea registers SIGINT, SIGTERM, SIGWINCH and SIGCONT but not SIGHUP, so
// a delivered hangup would take Go's default disposition and kill the process
// mid-render, leaving the terminal in whatever state the last escape sequence
// left it. Handling it turns that into an ordinary quit that restores the
// screen. It is deliberately NOT the answer to abandoned sessions — the two
// found running for days never received a hangup at all, which is why the
// liveness probe above exists.
func watchHangup(p *tea.Program) func() {
	if p == nil {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			p.Quit()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
