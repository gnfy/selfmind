package components

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/ui/common"
)

// SessionBrowser is a modal overlay for browsing past sessions.
type SessionBrowser struct {
	common     *common.Common
	viewport   viewport.Model
	sessions   []memory.FTS5Session
	searchQuery string
	selected   int
	closed     bool
	width, height int

	// External search function injected at creation time
	searchFn func(query string, limit int) (interface{}, error)
}

type SessionBrowserOption func(*SessionBrowser)

// WithSearchFn sets the search function for the session browser.
func WithSearchFn(fn func(query string, limit int) (interface{}, error)) SessionBrowserOption {
	return func(sb *SessionBrowser) { sb.searchFn = fn }
}

// NewSessionBrowser creates a new session browser modal.
func NewSessionBrowser(c *common.Common, width, height int, opts ...SessionBrowserOption) *SessionBrowser {
	vp := viewport.New(width, height-3)
	vp.Style = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1)

	sb := &SessionBrowser{
		common:     c,
		viewport:   vp,
		sessions:   []memory.FTS5Session{},
		searchQuery: "",
		selected:   0,
		width:      width,
		height:     height,
	}
	for _, opt := range opts {
		opt(sb)
	}
	return sb
}

// SetSearchFn sets the search function after creation.
func (sb *SessionBrowser) SetSearchFn(fn func(query string, limit int) (interface{}, error)) {
	sb.searchFn = fn
}

// LoadSessions fetches sessions from storage. Call with empty query for recent sessions.
func (sb *SessionBrowser) LoadSessions(query string) error {
	if sb.searchFn == nil {
		return fmt.Errorf("search function not set")
	}
	result, err := sb.searchFn(query, 50)
	if err != nil {
		return err
	}
	if result == nil {
		sb.sessions = []memory.FTS5Session{}
		return nil
	}
	switch v := result.(type) {
	case []memory.FTS5Session:
		sb.sessions = v
	case []interface{}:
		sb.sessions = make([]memory.FTS5Session, 0, len(v))
		for _, item := range v {
			if s, ok := item.(memory.FTS5Session); ok {
				sb.sessions = append(sb.sessions, s)
			}
		}
	}
	sb.selected = 0
	return nil
}

// Init implements tea.Model.
func (sb *SessionBrowser) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (sb *SessionBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			sb.closed = true
			return sb, tea.Quit
		case tea.KeyUp:
			if sb.selected > 0 {
				sb.selected--
			}
		case tea.KeyDown:
			if sb.selected < len(sb.sessions)-1 {
				sb.selected++
			}
		case tea.KeyEnter:
			if sb.selected >= 0 && sb.selected < len(sb.sessions) {
				sb.closed = true
				return sb, tea.Quit
			}
		case tea.KeyBackspace:
			if len(sb.searchQuery) > 0 {
				sb.searchQuery = sb.searchQuery[:len(sb.searchQuery)-1]
			}
		case tea.KeySpace:
			sb.searchQuery += " "
		default:
			if len(msg.Runes) > 0 {
				sb.searchQuery += string(msg.Runes[0])
			}
		}
		// Re-search on query change (debounce not needed for in-memory)
		if sb.searchQuery != "" || msg.Type == tea.KeyBackspace || msg.Type == tea.KeySpace {
			_ = sb.LoadSessions(sb.searchQuery)
		} else if sb.searchQuery == "" {
			_ = sb.LoadSessions("")
		}
	}
	return sb, nil
}

// View implements tea.Model.
func (sb *SessionBrowser) View() string {
	if sb.closed {
		return ""
	}

	s := sb.common.Styles

	// Header
	header := s.Sidebar.Panel.Copy().
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Padding(0, 1).
		Width(sb.width).
		Render("Session Browser  [/ for search]  [Esc to close]")

	// Search bar
	searchBar := "  Search: " + sb.searchQuery
	if sb.searchQuery == "" {
		searchBar = "  Search: (type to filter sessions, Enter to browse)"
	}
	searchLine := lipgloss.NewStyle().
		Foreground(lipgloss.Color("250")).
		Render(searchBar)

	// Session list
	var listLines []string
	if len(sb.sessions) == 0 {
		listLines = append(listLines, "  No sessions found.")
	}
	for i, sess := range sb.sessions {
		ts := time.Unix(sess.Timestamp, 0).Format("2006-01-02 15:04")
		content := sess.Content
		if len(content) > 80 {
			content = content[:80] + "..."
		}
		summary := sess.Summary
		if len(summary) > 60 {
			summary = summary[:60] + "..."
		}
		var prefixStr string
		if i == sb.selected {
			prefixStr = s.Sidebar.Item.Copy().
				Foreground(lipgloss.Color("202")).
				Render("▶")
		} else {
			prefixStr = "  "
		}
		listLines = append(listLines, fmt.Sprintf("%s[%s] %s — %s", prefixStr, ts, summary, content))
	}

	listContent := strings.Join(listLines, "\n")
	viewportContent := listContent
	if len(sb.sessions) > sb.viewport.Height {
		sb.viewport.SetContent(listContent)
		viewportContent = sb.viewport.View()
	}

	// Footer hint
	footer := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Render("  ↑↓ navigate  ·  Enter select  ·  Esc close")

	return fmt.Sprintf("%s\n%s\n%s\n%s",
		header, searchLine, viewportContent, footer)
}

// IsClosed returns true if the browser was closed (user pressed Esc or selected).
func (sb *SessionBrowser) IsClosed() bool {
	return sb.closed
}

// SelectedSession returns the currently selected session, or nil.
func (sb *SessionBrowser) SelectedSession() *memory.FTS5Session {
	if sb.selected >= 0 && sb.selected < len(sb.sessions) {
		return &sb.sessions[sb.selected]
	}
	return nil
}
