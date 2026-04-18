package sidebar

import (
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/layout"
)

type Sidebar struct {
	common  *common.Common
	content string
}

func New(c *common.Common) *Sidebar {
	return &Sidebar{common: c, content: ""}
}

func (s *Sidebar) SetContent(c string) {
	s.content = c
}

func (s *Sidebar) Draw(rect layout.Rect) string {
	return s.common.Styles.Sidebar.Panel.
		Width(rect.W).
		Height(rect.H).
		Render(s.content)
}
