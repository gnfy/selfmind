package cli

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestQuitBlanksActiveRegion pins the exit render contract. Bubbletea repaints
// the model one final time on a graceful shutdown, so a quit path that leaves
// the composer in View strands an empty input box between the transcript and
// the resume hint (observed live on /exit and ctrl+c). The final frame must be
// one blank line: an empty string makes the renderer skip the frame entirely,
// leaving the previous composer painted.
func TestQuitBlanksActiveRegion(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width, model.height = 100, 30

	if view := stripANSI(model.View()); !strings.Contains(view, "Ask SelfMind") {
		t.Fatalf("composer must render while the session is live: %q", view)
	}

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = updated.(*uiModel)
	if cmd == nil {
		t.Fatal("ctrl+c on an idle session must return the quit command")
	}
	if !model.quitting {
		t.Fatal("the quit path must mark the model as quitting")
	}

	view := model.View()
	if view == "" {
		t.Fatal("final frame must be one blank line, not empty: the renderer skips an empty buffer and keeps the old frame")
	}
	if strings.Contains(stripANSI(view), "Ask SelfMind") {
		t.Fatalf("composer must not survive the final repaint: %q", view)
	}
	if strings.TrimSpace(stripANSI(view)) != "" {
		t.Fatalf("final frame must carry no visible content: %q", view)
	}
	if strings.Contains(view, "\n") {
		t.Fatalf("final frame must be a single line so the renderer's stop() erases it: %q", view)
	}
}

// TestExitCommandBlanksActiveRegion covers the /exit slash command, which is a
// separate quit path from ctrl+c and regressed independently.
func TestExitCommandBlanksActiveRegion(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width, model.height = 100, 30

	var exit *slashCommand
	for i := range allSlashCommands {
		if allSlashCommands[i].Name == "/exit" {
			exit = &allSlashCommands[i]
			break
		}
	}
	if exit == nil {
		t.Fatal("/exit must be a registered slash command")
	}
	if cmd := exit.Run(model, nil); cmd == nil {
		t.Fatal("/exit must return the quit command")
	}
	if !model.quitting {
		t.Fatal("/exit must mark the model as quitting")
	}
	if strings.Contains(stripANSI(model.View()), "Ask SelfMind") {
		t.Fatalf("composer must not survive /exit: %q", model.View())
	}
}
