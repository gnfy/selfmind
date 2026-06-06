package components

import (
	"fmt"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"selfmind/internal/ui/common"
)

// PagerContent builds the current page content for the available terminal width.
type PagerContent func(width int) string

// Pager is a reusable full-screen transient surface for help, detail, and
// inspection views that should not become part of the main transcript.
type Pager struct {
	common   *common.Common
	viewport viewport.Model
	width    int
	height   int
	build    PagerContent
}

func NewPager(c *common.Common, width, height int, build PagerContent) *Pager {
	p := &Pager{
		common: c,
		build:  build,
	}
	p.Resize(width, height)
	p.viewport.GotoTop()
	return p
}

func (p *Pager) Resize(width, height int) {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	p.width = width
	p.height = height
	p.viewport.Width = width
	p.viewport.Height = maxInt(1, height-1)
	p.refresh()
}

func (p *Pager) Update(msg tea.Msg) (closed bool, cmd tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return true, nil
		}
	}

	p.viewport, cmd = p.viewport.Update(msg)
	return false, cmd
}

func (p *Pager) View() string {
	p.refresh()
	footerText := fmt.Sprintf(" line %d  q/Esc close - up/down scroll", p.viewport.YOffset+1)
	footerStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("255")).
		Foreground(lipgloss.Color("0")).
		Width(p.width)
	if p.common != nil && p.common.Styles != nil && p.common.Width > 0 {
		footerStyle = footerStyle.MaxWidth(p.common.Width)
	}
	footer := footerStyle.Render(footerText)
	return lipgloss.JoinVertical(lipgloss.Left, p.viewport.View(), footer)
}

func (p *Pager) refresh() {
	if p.build == nil {
		return
	}
	p.viewport.SetContent(p.build(p.width))
	p.viewport.SetYOffset(p.viewport.YOffset)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
