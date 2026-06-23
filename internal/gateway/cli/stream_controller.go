package cli

import "strings"

// markdownStreamController buffers raw markdown deltas and exposes stable
// chunks. Complete lines are safe to render immediately; partial lines are
// flushed by a short UI timer so long paragraphs still feel live.
type markdownStreamController struct {
	pending strings.Builder
}

func (c *markdownStreamController) Push(delta string) string {
	if delta == "" {
		return ""
	}
	c.pending.WriteString(delta)
	pending := c.pending.String()
	idx := strings.LastIndex(pending, "\n")
	if idx < 0 {
		return ""
	}
	commit := pending[:idx+1]
	rest := pending[idx+1:]
	c.pending.Reset()
	c.pending.WriteString(rest)
	return commit
}

func (c *markdownStreamController) Flush() string {
	if c.pending.Len() == 0 {
		return ""
	}
	out := c.pending.String()
	c.pending.Reset()
	return out
}

func (c *markdownStreamController) Pending() bool {
	return c.pending.Len() > 0
}

func (c *markdownStreamController) Reset() {
	c.pending.Reset()
}
