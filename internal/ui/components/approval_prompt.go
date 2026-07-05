package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// ApprovalOption is one selectable answer in an ApprovalPrompt panel. Decision
// and Scope mirror the gateway approval-respond contract
// (api.ApprovalRespondRequest): Decision is "approved"|"rejected"; Scope is the
// class-grant memory scope — "" (once), "task", or "person".
type ApprovalOption struct {
	Label    string
	Key      string // single-key shortcut ("y", "t", "a", "n")
	Decision string
	Scope    string
}

// DefaultApprovalOptions is the Codex-style answer set for a tool approval:
// approve once, approve + remember for this task, approve + remember for the
// person, or deny (with optional follow-up guidance, handled by the caller).
func DefaultApprovalOptions() []ApprovalOption {
	return []ApprovalOption{
		{Label: "Yes, run it once", Key: "y", Decision: "approved", Scope: ""},
		{Label: "Yes, and allow this kind for this task", Key: "t", Decision: "approved", Scope: "task"},
		{Label: "Yes, always allow this kind", Key: "a", Decision: "approved", Scope: "person"},
		{Label: "No, and tell the agent what to do instead", Key: "n", Decision: "rejected", Scope: ""},
	}
}

// ApprovalPrompt is a bordered, selectable approval panel rendered in the TUI's
// active region while a run is blocked on a tool approval. It is a passive
// component: the controller feeds it keys via HandleKey and draws it via View —
// it owns no timers, IO, or approval lifecycle state (that stays in the gateway
// per docs/architecture-constraints.md).
type ApprovalPrompt struct {
	tool    string
	target  string // compact object of the action (path/command); may be empty
	reason  string
	options []ApprovalOption
	cursor  int
}

// NewApprovalPrompt builds a panel with the default option set.
func NewApprovalPrompt(tool, target, reason string) *ApprovalPrompt {
	return &ApprovalPrompt{
		tool:    strings.TrimSpace(tool),
		target:  strings.TrimSpace(target),
		reason:  strings.TrimSpace(reason),
		options: DefaultApprovalOptions(),
	}
}

// Tool returns the tool name the pending approval is for.
func (p *ApprovalPrompt) Tool() string { return p.tool }

// Cursor returns the highlighted option index (for tests).
func (p *ApprovalPrompt) Cursor() int { return p.cursor }

// Options returns the option set (for tests and callers resolving a choice).
func (p *ApprovalPrompt) Options() []ApprovalOption { return p.options }

// HandleKey processes one key press. It returns the chosen option when the key
// resolves the prompt (Enter on the highlighted row, or a shortcut key), and
// nil otherwise. Up/Down and j/k move the selector. Esc deliberately does
// NOTHING: an approval must be an explicit decision, never dismissed
// implicitly. All other keys are ignored.
func (p *ApprovalPrompt) HandleKey(key string) *ApprovalOption {
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
		opt := p.options[p.cursor]
		return &opt
	case "esc":
		return nil
	}
	lower := strings.ToLower(key)
	for _, opt := range p.options {
		if opt.Key != "" && opt.Key == lower {
			opt := opt
			return &opt
		}
	}
	return nil
}

var (
	approvalBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	approvalTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	approvalToolStyle   = lipgloss.NewStyle().Bold(true)
	approvalDimStyle    = lipgloss.NewStyle().Faint(true)
	approvalSelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
)

// approvalPanelMaxWidth caps the panel so it stays a compact dialog even in a
// very wide terminal.
const approvalPanelMaxWidth = 76

// View renders the bordered panel at (up to) the given terminal width. Long
// values (paths, commands) are middle-truncated so the panel never wraps.
func (p *ApprovalPrompt) View(width int) string {
	panelW := width
	if panelW > approvalPanelMaxWidth {
		panelW = approvalPanelMaxWidth
	}
	if panelW < 30 {
		panelW = 30
	}
	inner := panelW - 4 // "│ " + " │"

	type row struct {
		text  string // styled content
		width int    // display width of the unstyled content
	}
	var rows []row
	addRow := func(styled string, plain string) {
		rows = append(rows, row{text: styled, width: runewidth.StringWidth(plain)})
	}

	// Header: tool name, with the target's base name when the target is a path
	// ("write_file → report.html"); the full target gets its own line below when
	// it carries more than the base name.
	head := p.tool
	headStyled := approvalToolStyle.Render(p.tool)
	if p.target != "" {
		base := pathBaseName(p.target)
		shown := TruncateMiddle(base, maxInt(8, inner-runewidth.StringWidth(head)-3))
		head += " → " + shown
		headStyled += approvalDimStyle.Render(" → ") + shown
	}
	head = TruncateMiddle(head, inner)
	addRow(headStyled, head)
	if p.target != "" && p.target != pathBaseName(p.target) {
		full := TruncateMiddle(p.target, inner)
		addRow(approvalDimStyle.Render(full), full)
	}
	if p.reason != "" {
		reason := TruncateMiddle(p.reason, inner-8)
		addRow(approvalDimStyle.Render("reason: ")+reason, "reason: "+reason)
	}
	addRow("", "")

	for i, opt := range p.options {
		marker := "  "
		style := lipgloss.NewStyle()
		if i == p.cursor {
			marker = "❯ "
			style = approvalSelStyle
		}
		hint := ""
		if opt.Key != "" {
			hint = "(" + opt.Key + ")"
		}
		label := TruncateMiddle(opt.Label, maxInt(8, inner-2-len(hint)-1))
		pad := inner - 2 - runewidth.StringWidth(label) - len(hint)
		if pad < 1 {
			pad = 1
		}
		plain := marker + label + strings.Repeat(" ", pad) + hint
		styled := style.Render(marker+label) + strings.Repeat(" ", pad) + approvalDimStyle.Render(hint)
		addRow(styled, plain)
	}
	footer := "↑/↓ move · enter select · shortcuts answer directly"
	footer = TruncateMiddle(footer, inner)
	addRow(approvalDimStyle.Render(footer), footer)

	title := " Approval required "
	dashes := panelW - 3 - runewidth.StringWidth(title)
	if dashes < 0 {
		dashes = 0
	}
	var sb strings.Builder
	sb.WriteString(approvalBorderStyle.Render("╭─") + approvalTitleStyle.Render(title) +
		approvalBorderStyle.Render(strings.Repeat("─", dashes)+"╮") + "\n")
	for _, r := range rows {
		pad := inner - r.width
		if pad < 0 {
			pad = 0
		}
		sb.WriteString(approvalBorderStyle.Render("│ ") + r.text + strings.Repeat(" ", pad) +
			approvalBorderStyle.Render(" │") + "\n")
	}
	sb.WriteString(approvalBorderStyle.Render("╰" + strings.Repeat("─", panelW-2) + "╯"))
	return sb.String()
}

// pathBaseName returns the last path segment of a slash- or backslash-separated
// value; non-path values return themselves.
func pathBaseName(s string) string {
	idx := strings.LastIndexAny(strings.TrimRight(s, "/\\"), "/\\")
	if idx < 0 {
		return s
	}
	base := strings.TrimRight(s, "/\\")[idx+1:]
	if base == "" {
		return s
	}
	return base
}

// TruncateMiddle shortens a string to at most max display columns by removing
// the middle ("head…tail"), preserving the informative start and end of paths
// and commands. Exported for callers that need the same treatment outside the
// panel (e.g. compact transcript records).
func TruncateMiddle(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	keep := max - 1
	leftW := keep / 2
	rightW := keep - leftW
	runes := []rune(s)
	var left strings.Builder
	used := 0
	for _, r := range runes {
		w := runewidth.RuneWidth(r)
		if used+w > leftW {
			break
		}
		left.WriteRune(r)
		used += w
	}
	var rightRunes []rune
	used = 0
	for i := len(runes) - 1; i >= 0; i-- {
		w := runewidth.RuneWidth(runes[i])
		if used+w > rightW {
			break
		}
		rightRunes = append([]rune{runes[i]}, rightRunes...)
		used += w
	}
	return left.String() + "…" + string(rightRunes)
}
