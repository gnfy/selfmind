package cli

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestEnterAcceptsHighlightedSuggestion pins the CLI UX fix: with the
// slash-command popup open on a partial like "/m", pressing Enter must accept
// the highlighted command and run it — not submit "/m" and report "Unknown
// command". The user should never have to type the command in full.
func TestEnterAcceptsHighlightedSuggestion(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	model.height = 30

	model.editor.SetValue("/m")
	if !model.editor.SuggestionsVisible() {
		t.Fatal("popup should be visible for the '/m' prefix")
	}

	// Enter (default-case key handling matches on msg.String() == "enter").
	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)

	// The echoed user cell must be a real command (the first '/m…' match), never
	// the bare "/m", and there must be no "Unknown command" reply.
	var lastUser, lastAssistant string
	for _, msg := range model.messages {
		switch msg.Role {
		case "user":
			lastUser = msg.Content
		case "assistant":
			lastAssistant = msg.Content
		}
	}
	accepted := strings.TrimSpace(lastUser)
	if accepted == "/m" || accepted == "" {
		t.Fatalf("Enter should have accepted a full command, got echoed %q", lastUser)
	}
	if !strings.HasPrefix(accepted, "/m") || len(accepted) <= len("/m") {
		t.Fatalf("accepted command should be a completed '/m…' command, got %q", accepted)
	}
	if strings.Contains(lastAssistant, "Unknown command") {
		t.Fatalf("Enter accepted an invalid command: %q", lastAssistant)
	}
}

// TestEnterWithArgsDoesNotClobber guards the regression boundary: once a space
// is typed the popup closes, so Enter submits the full "/command args" verbatim
// (AcceptSuggestion never fires).
func TestEnterWithArgsDoesNotClobber(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	model.height = 30

	model.editor.SetValue("/task 3 rename foo")
	if model.editor.SuggestionsVisible() {
		t.Fatal("popup must be closed once the input contains a space")
	}

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)

	var lastUser string
	for _, msg := range model.messages {
		if msg.Role == "user" {
			lastUser = msg.Content
		}
	}
	if strings.TrimSpace(lastUser) != "/task 3 rename foo" {
		t.Fatalf("command with args must submit verbatim, got %q", lastUser)
	}
}
