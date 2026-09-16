package components

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	uitheme "selfmind/internal/ui/theme"
)

// RuntimeSetup contains only a proposed choice. Its owner validates paths and
// installs/trusts anything only after the person chooses Start SelfMind.
type RuntimeSetup struct {
	Workspace, ApprovalMode, Protection string
	Managed, ManagedAvailable, Trusted  bool
	Accepted, BackToModels              bool
	ValidateWorkspace                   func(string) (string, error)
	theme                               uitheme.Theme
	index, width                        int
	screen                              string
	input                               []rune
	message                             string
}

func NewRuntimeSetup(workspace, approval, protection string, managed, supported bool, theme uitheme.Theme, validate func(string) (string, error)) *RuntimeSetup {
	return &RuntimeSetup{Workspace: workspace, ApprovalMode: approval, Protection: protection, Managed: managed, ManagedAvailable: supported, Trusted: true, ValidateWorkspace: validate, theme: theme, index: 4, width: 80}
}

func (m *RuntimeSetup) Init() tea.Cmd { return nil }

func (m *RuntimeSetup) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if key.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.screen == "workspace" {
		return m.updatePath(key)
	}
	switch key.String() {
	case "esc", "left", "backspace":
		if m.screen != "" {
			m.screen, m.index = "", 2
			return m, nil
		}
		m.BackToModels = true
		return m, tea.Quit
	case "up", "k":
		m.index = (m.index + len(m.options()) - 1) % len(m.options())
	case "down", "j":
		m.index = (m.index + 1) % len(m.options())
	case "enter":
		return m.choose()
	}
	return m, nil
}

func (m *RuntimeSetup) choose() (tea.Model, tea.Cmd) {
	if m.screen == "safety" {
		m.ApprovalMode = []string{"smart", "on-request", "auto-edit"}[m.index]
		m.screen, m.index = "", 2
		return m, nil
	}
	m.message = ""
	switch m.index {
	case 0:
		m.screen, m.input = "workspace", []rune(m.Workspace)
	case 1:
		m.Trusted = !m.Trusted
	case 2:
		m.screen, m.index = "safety", 0
	case 3:
		if m.ManagedAvailable {
			m.Managed = !m.Managed
		} else {
			m.message = "Automatic startup is unavailable; the daemon starts on demand."
		}
	case 4:
		if !m.Trusted {
			m.message = "Trust this project or choose another project directory."
			return m, nil
		}
		if !m.validatePath(m.Workspace) {
			return m, nil
		}
		m.Accepted = true
		return m, tea.Quit
	case 5:
		m.BackToModels = true
		return m, tea.Quit
	case 6:
		return m, tea.Quit
	}
	return m, nil
}

func (m *RuntimeSetup) validatePath(path string) bool {
	if m.ValidateWorkspace == nil {
		m.message = "Workspace validation is unavailable."
		return false
	}
	canonical, err := m.ValidateWorkspace(path)
	if err != nil {
		m.message = err.Error()
		return false
	}
	m.Workspace = canonical
	return true
}

func (m *RuntimeSetup) updatePath(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.screen, m.index, m.message = "", 0, ""
	case "enter":
		if m.validatePath(string(m.input)) {
			m.screen, m.index, m.message = "", 0, ""
		}
	case "backspace":
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	case "ctrl+u":
		m.input = nil
	default:
		if len(key.Runes) > 0 {
			m.input = append(m.input, key.Runes...)
		}
	}
	return m, nil
}

func (m *RuntimeSetup) options() []string {
	if m.screen == "safety" {
		return []string{"Smart", "On request", "Automatic edits"}
	}
	trust, startup := "No", "No (daemon starts on demand)"
	if m.Trusted {
		trust = "Yes"
	}
	if m.Managed {
		startup = "Yes"
	}
	return []string{"Workspace       " + m.Workspace, "Trust project   " + trust, "Safety          " + m.ApprovalMode, "Start at login  " + startup, "Start SelfMind", "Back to models", "Cancel"}
}

func (m *RuntimeSetup) View() string {
	muted := lipgloss.NewStyle().Foreground(m.theme.Color(uitheme.TextSecondary))
	selected := lipgloss.NewStyle().Foreground(m.theme.Color(uitheme.SelectionText)).Background(m.theme.Color(uitheme.SelectionBackground))
	lines := []string{"", "SelfMind setup · Workspace", "", "Start with these settings?", ""}
	if m.screen == "workspace" {
		lines = append(lines, "Project directory", string(m.input)+"█", "", muted.Render("Enter confirm  Ctrl+U clear  Esc back"))
	} else {
		for i, option := range m.options() {
			line := "  " + option
			if i == m.index {
				line = selected.Render("› " + option)
			}
			lines = append(lines, line)
		}
		startup := "The daemon starts on demand; no login service is installed."
		if m.Managed {
			startup = "Start installs a user-level login service."
		}
		lines = append(lines, "", muted.Render("Repository instructions are trusted; tool approvals still apply."), muted.Render(startup), muted.Render("Protection: "+m.Protection), "", muted.Render("↑/↓ move  Enter select  Esc back to models  Ctrl+C cancel"))
	}
	if m.message != "" {
		lines = append(lines, "", fmt.Sprintf("! %s", m.message))
	}
	view := strings.Join(lines, "\n")
	if m.width > 4 {
		view = lipgloss.NewStyle().Width(m.width - 2).Render(view)
	}
	return view
}
