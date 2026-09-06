package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	uitheme "selfmind/internal/ui/theme"
)

// WorkspaceTrustOption is one answer to the workspace trust question. Command
// is the explicit gateway control the answer maps to, or "" for an answer the
// client settles by itself (deferring keeps the daemon's record untouched, so
// the question returns next time).
type WorkspaceTrustOption struct {
	Label   string
	Key     string
	Command string
}

// WorkspaceTrustPrompt asks the one-time workspace trust question as a question.
//
// It used to ride the startup digest as a line of prose. Trust is not urgent
// and nothing is blocked without it, so a notice looked like the proportionate
// choice — but it arrives among the startup card, tips, and the digest, and a
// line that never asks for an answer is a line people read past. The two
// capabilities then stay off for the life of the workspace without anyone
// having decided that. Deferring is one keystroke, so the interruption costs
// about what the notice did while actually collecting an answer.
//
// The component is passive: the controller feeds it keys and draws it. The
// answers are the gateway's own explicit controls; this panel never invents a
// trust level of its own.
type WorkspaceTrustPrompt struct {
	name    string
	path    string
	options []WorkspaceTrustOption
	cursor  int
	styles  approvalStyles
}

func NewWorkspaceTrustPrompt(name, path string, t uitheme.Theme) *WorkspaceTrustPrompt {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "this workspace"
	}
	return &WorkspaceTrustPrompt{
		name: name, path: strings.TrimSpace(path), styles: newApprovalStyles(t),
		options: []WorkspaceTrustOption{
			{Label: "Trust " + name, Key: "t", Command: "/ws trust"},
			{Label: "Not now — ask again next time", Key: "n"},
			{Label: "Never for " + name + " — stay untrusted and stop asking", Key: "d", Command: "/ws decline"},
		},
	}
}

func (p *WorkspaceTrustPrompt) Options() []WorkspaceTrustOption {
	if p == nil {
		return nil
	}
	return p.options
}

func (p *WorkspaceTrustPrompt) Cursor() int {
	if p == nil {
		return 0
	}
	return p.cursor
}

// HandleKey returns the chosen answer, or nil while the person is still
// deciding. Esc answers "not now": dismissing must stay cheap, and a dismissal
// is not a decision the daemon should record.
func (p *WorkspaceTrustPrompt) HandleKey(key string) *WorkspaceTrustOption {
	if p == nil {
		return nil
	}
	switch key {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
		return nil
	case "down", "j":
		if p.cursor < len(p.options)-1 {
			p.cursor++
		}
		return nil
	case "enter":
		option := p.options[p.cursor]
		return &option
	case "esc":
		return &WorkspaceTrustOption{Label: "Not now — ask again next time", Key: "n"}
	}
	lower := strings.ToLower(key)
	for _, option := range p.options {
		if option.Key != "" && option.Key == lower {
			option := option
			return &option
		}
	}
	return nil
}

func (p *WorkspaceTrustPrompt) View(width int) string {
	if p == nil {
		return ""
	}
	panelW := width
	if panelW < 12 {
		panelW = 12
	}
	inner := min(panelW, approvalPanelMaxWidth)

	var rows []string
	addRow := func(styled string) { rows = append(rows, styled) }

	addRow(p.styles.title.Render("Trust this workspace?"))
	if p.path != "" {
		addRow(p.styles.target.Render(TruncateMiddle(p.name+" · "+p.path, inner)))
	}
	addRow("")
	// Say what the answer buys and what it does not. Untrusted is a capability
	// fact, not a warning about danger, and a trust question that implies danger
	// pushes people toward a yes they did not mean.
	for _, line := range WrapDisplay("Trusting lets this workspace load its own Skills and reuse approval observations remembered here. Everything else runs the same either way.", inner, 3) {
		addRow(p.styles.secondary.Render(line))
	}
	addRow("")

	for i, option := range p.options {
		marker := "  "
		style := lipgloss.NewStyle()
		if i == p.cursor {
			marker = "❯ "
			style = p.styles.selected
		}
		hint := ""
		if option.Key != "" {
			hint = "(" + option.Key + ") "
		}
		lineWidth := maxInt(8, inner-runewidth.StringWidth(marker)-runewidth.StringWidth(hint))
		for lineIndex, line := range WrapDisplay(option.Label, lineWidth, 2) {
			if lineIndex == 0 {
				addRow(style.Render(marker) + p.styles.secondary.Render(hint) + style.Render(line))
				continue
			}
			addRow(strings.Repeat(" ", runewidth.StringWidth(marker)+runewidth.StringWidth(hint)) + style.Render(line))
		}
	}
	addRow("")
	addRow(p.styles.secondary.Render(TruncateMiddle("↑/↓ to move · enter to confirm · esc to decide later", inner)))
	return strings.Join(rows, "\n")
}
