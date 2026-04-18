package status

import (
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/layout"
)

type Status struct {
	common *common.Common
	msg    string
}

func New(c *common.Common) *Status {
	return &Status{common: c, msg: "Ready"}
}

func (s *Status) SetMsg(msg string) {
	s.msg = msg
}

func (s *Status) Draw(rect layout.Rect) string {
	return s.common.Styles.Status.Panel.
		Width(rect.W).
		Render(s.msg)
}
