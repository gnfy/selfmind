package components

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	uitheme "selfmind/internal/ui/theme"
)

func TestRuntimeSetupTrustAndWorkspaceGate(t *testing.T) {
	m := NewRuntimeSetup("/home/person", "smart", "host approvals", true, true, uitheme.Default(), func(path string) (string, error) {
		if path != "/project" {
			return "", fmt.Errorf("choose a project")
		}
		return path, nil
	})
	key := func(kind tea.KeyType) tea.Cmd { _, cmd := m.Update(tea.KeyMsg{Type: kind}); return cmd }
	if cmd := key(tea.KeyEnter); cmd != nil || m.Accepted {
		t.Fatal("home directory was accepted")
	}
	m.index = 0
	key(tea.KeyEnter)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/project")})
	key(tea.KeyEnter)
	m.index = 1
	key(tea.KeyEnter)
	m.index = 4
	if cmd := key(tea.KeyEnter); cmd != nil || m.Accepted {
		t.Fatal("untrusted project was accepted")
	}
	m.index = 1
	key(tea.KeyEnter)
	m.index = 3
	key(tea.KeyEnter)
	m.index = 4
	if cmd := key(tea.KeyEnter); cmd == nil || !m.Accepted || m.Managed {
		t.Fatal("explicit on-demand setup was not accepted")
	}
}

func TestRuntimeSetupBackDoesNotAcceptSettings(t *testing.T) {
	m := NewRuntimeSetup("/project", "smart", "host approvals", false, false, uitheme.Default(), nil)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil || !m.BackToModels || m.Accepted {
		t.Fatal("back must return to model setup without authorizing side effects")
	}
}
