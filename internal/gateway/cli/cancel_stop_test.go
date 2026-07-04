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

func TestCtrlCRoutesCancelThroughStopCommand(t *testing.T) {
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

	if !cancelled {
		t.Fatal("ctrl+c must still cancel the local watcher ctx")
	}
	if m.thinking || m.runStatus != "cancelled" {
		t.Fatalf("thinking=%v runStatus=%q, want cancelled UI state", m.thinking, m.runStatus)
	}
	if cmd == nil {
		t.Fatal("ctrl+c returned no command; /stop was never dispatched")
	}
	runCmdTree(cmd)
	select {
	case content := <-got:
		if !strings.HasPrefix(content, "/stop") {
			t.Fatalf("dispatched %q, want /stop", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl+c did not dispatch /stop through the message processor")
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
