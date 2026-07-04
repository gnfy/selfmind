package cli

// G0-a: runs are daemon-owned and detached from the endpoint's ctx, so the
// TUI's ctrl+c must route the actual cancellation through the /stop control
// command (registry-backed), not just cancel the local watcher ctx.

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
)

// runCmdTree executes a tea.Cmd and recursively drains BatchMsg/sequence
// wrappers in goroutines, so tick commands don't block the test.
func runCmdTree(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				runCmdTree(sub)
			}
		}
	}()
}

// TestCtrlCShowsExitPromptThenCancelViaC: with a run active, ctrl+c must NOT
// cancel or quit — it shows the exit prompt (teaching the detached-run design);
// pressing c then cancels through the registry-backed /stop and stays.
func TestCtrlCShowsExitPromptThenCancelViaC(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	got := make(chan string, 4)
	model.messageProcessor = func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		got <- req.Content
		return api.MessageResponse{Content: "Stopping run run_x."}, 200
	}

	cancelled := false
	model.cancelFn = func() { cancelled = true }
	model.thinking = true

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m := updated.(*uiModel)
	if cancelled {
		t.Fatal("first ctrl+c must not cancel — it should only show the exit prompt")
	}
	if !m.exitPromptActive {
		t.Fatal("exit prompt should be active after ctrl+c during a run")
	}
	if cmd != nil {
		t.Fatal("showing the prompt must not quit or dispatch anything")
	}
	last := m.messages[len(m.messages)-1]
	if !strings.Contains(last.Content, "background") || !strings.Contains(last.Content, "cancel") {
		t.Fatalf("prompt should explain the choices: %q", last.Content)
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = updated.(*uiModel)
	if !cancelled {
		t.Fatal("c must cancel the local watcher ctx")
	}
	if m.exitPromptActive {
		t.Fatal("prompt should dismiss after choosing")
	}
	if m.thinking || m.runStatus != "cancelled" {
		t.Fatalf("thinking=%v runStatus=%q, want cancelled UI state", m.thinking, m.runStatus)
	}
	if cmd == nil {
		t.Fatal("c returned no command; /stop was never dispatched")
	}
	runCmdTree(cmd)
	select {
	case content := <-got:
		if !strings.HasPrefix(content, "/stop") {
			t.Fatalf("dispatched %q, want /stop", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("c did not dispatch /stop through the message processor")
	}
}

// TestExitPromptBackgroundQuitDoesNotStop: choosing b (or a second ctrl+c)
// quits while leaving the daemon-owned run untouched — no /stop, no cancel.
func TestExitPromptBackgroundQuitDoesNotStop(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	stopDispatched := make(chan string, 2)
	model.messageProcessor = func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		stopDispatched <- req.Content
		return api.MessageResponse{}, 200
	}
	cancelled := false
	model.cancelFn = func() { cancelled = true }
	model.thinking = true

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m := updated.(*uiModel)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = updated.(*uiModel)
	if cancelled {
		t.Fatal("background quit must not cancel the run")
	}
	if cmd == nil {
		t.Fatal("b should quit")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("expected tea.Quit message")
	}
	select {
	case content := <-stopDispatched:
		t.Fatalf("background quit must not dispatch anything, got %q", content)
	case <-time.After(200 * time.Millisecond):
	}

	// esc keeps watching.
	m.exitPromptActive = true
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(*uiModel)
	if m.exitPromptActive {
		t.Fatal("esc should dismiss the prompt")
	}
}

// TestRequestDaemonStopIsNoOpWithoutProcessor covers the legacy in-process
// agent path, where the local ctx still owns the run and no /stop exists.
func TestRequestDaemonStopIsNoOpWithoutProcessor(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.messageProcessor = nil
	if cmd := model.requestDaemonStop(); cmd != nil {
		t.Fatal("requestDaemonStop must be a no-op without a message processor")
	}
}
