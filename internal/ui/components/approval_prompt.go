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
	Key      string // single-key shortcut ("y", "t", "a", "n", or a rule letter)
	Decision string
	Scope    string
	// GrantKey names a narrow RULE this option would persist ("commands that
	// start with `git status`"). It is opaque here: the panel renders the label
	// and hands the key back untouched, because only the daemon may decide
	// whether a rule is honored.
	GrantKey string
	// RuleLabel is the rule in plain words, used for the transcript record so it
	// says what was remembered rather than a scope noun.
	RuleLabel string
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
	details ApprovalDetails
	options []ApprovalOption
	cursor  int
}

// ApprovalDetails is the decision context the daemon publishes with an approval:
// WHERE the operation would run, HOW LARGE the write is, WHAT a "remember this"
// answer would authorize, and WHETHER automatic triage was able to rule at all.
// Every field is display-only — none of it widens what the approval authorizes —
// and every field is optional, because non-exec approvals and older daemons
// carry only some of it.
type ApprovalDetails struct {
	Tool   string
	Target string
	Reason string
	// Environment is the bound environment snapshot id; Cwd is the working root
	// the operation would run in (the scope's root, never the daemon cwd).
	Environment string
	Cwd         string
	// ChangeSummary is a content-free size line ("2 files +48/-12").
	ChangeSummary string
	// GrantClass names what "allow this kind" would authorize, or "" when the
	// daemon did not publish one. It is rendered, not acted on: whether a grant
	// is actually persisted is the grant floor's decision, so the panel must not
	// infer an option set from it.
	GrantClass string
	// TriageUnavailable is true when smart-mode triage could not rule (no judge,
	// error, timeout). The panel says so, because otherwise a broken judge and a
	// strict judge are indistinguishable and the person just sees more prompts.
	TriageUnavailable bool
	// TriageRationale and TriageRisk are the judge's assessment when triage ran
	// and escalated. Showing them means the person inherits the reasoning instead
	// of redoing it.
	TriageRationale string
	TriageRisk      string
	// Options is the server-issued answer set for THIS ask (batch B1). Nil falls
	// back to the built-in four so an older daemon still renders a usable panel;
	// the panel never invents an option the daemon did not offer.
	Options []ApprovalOption
}

// NewApprovalPrompt builds a panel with the default option set and no extra
// decision context.
func NewApprovalPrompt(tool, target, reason string) *ApprovalPrompt {
	return NewApprovalPromptDetailed(ApprovalDetails{Tool: tool, Target: target, Reason: reason})
}

// NewApprovalPromptDetailed builds a panel that also renders execution context.
// The option set is unchanged: what a decision authorizes is the gateway's and
// the grant floor's contract, and the panel must not narrow it locally.
func NewApprovalPromptDetailed(d ApprovalDetails) *ApprovalPrompt {
	d.Tool = strings.TrimSpace(d.Tool)
	d.Target = strings.TrimSpace(d.Target)
	d.Reason = strings.TrimSpace(d.Reason)
	d.Environment = strings.TrimSpace(d.Environment)
	d.Cwd = strings.TrimSpace(d.Cwd)
	d.ChangeSummary = strings.TrimSpace(d.ChangeSummary)
	d.GrantClass = strings.TrimSpace(d.GrantClass)
	options := d.Options
	if len(options) == 0 {
		options = DefaultApprovalOptions()
	}
	return &ApprovalPrompt{
		tool:    d.Tool,
		target:  d.Target,
		reason:  d.Reason,
		details: d,
		options: options,
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

// approvalTargetMaxLines and approvalContextMaxLines bound the wrapped blocks so
// a long command or reason can never push the answer options off screen — the
// panel must always end with a visible decision.
const (
	approvalTargetMaxLines  = 3
	approvalContextMaxLines = 2
)

// approvalTriageUnavailableNotice is shown when smart-mode triage could not rule
// on the call. Without it, a person in smart mode reads a flood of prompts as
// "the policy is strict" when the real cause is a missing or failing judge.
const approvalTriageUnavailableNotice = "automatic triage unavailable — asking you instead of auto-approving"

var (
	approvalBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	approvalTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	approvalToolStyle   = lipgloss.NewStyle().Bold(true)
	approvalDimStyle    = lipgloss.NewStyle().Faint(true)
	approvalSelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	approvalNoticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// locationLine renders WHERE the operation would run: the working root, plus the
// bound environment snapshot when the daemon published one.
func (p *ApprovalPrompt) locationLine() string {
	var parts []string
	if p.details.Cwd != "" {
		parts = append(parts, p.details.Cwd)
	}
	if p.details.Environment != "" {
		parts = append(parts, "env "+p.details.Environment)
	}
	return strings.Join(parts, " · ")
}

// addLabeledRows appends "label: value" with the value wrapped and continuation
// lines indented under the label, so a wrapped path stays visually attached to
// the field that owns it.
func (p *ApprovalPrompt) addLabeledRows(addRow func(styled, plain string), label, value string, inner, maxLines int) {
	labelW := runewidth.StringWidth(label)
	lines := WrapDisplay(value, maxInt(8, inner-labelW), maxLines)
	if len(lines) == 0 {
		return
	}
	addRow(approvalDimStyle.Render(label)+lines[0], label+lines[0])
	indent := strings.Repeat(" ", labelW)
	for _, line := range lines[1:] {
		addRow(indent+line, indent+line)
	}
}

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
	// The full object gets WRAPPED, not middle-truncated: a command whose middle
	// is replaced by "…" cannot be judged, and judging it is the entire point of
	// the panel. Wrapping stays bounded (approvalTargetMaxLines) so one giant
	// argument list can never push the options off screen.
	if p.target != "" && p.target != pathBaseName(p.target) {
		for _, line := range WrapDisplay(p.target, inner, approvalTargetMaxLines) {
			addRow(approvalDimStyle.Render(line), line)
		}
	}
	// Execution context: where it runs, and how big the write is. Order matters —
	// location before size before rationale, because "wrong directory" is the
	// cheapest rejection to spot.
	if p.details.ChangeSummary != "" {
		p.addLabeledRows(addRow, "change: ", p.details.ChangeSummary, inner, 1)
	}
	if where := p.locationLine(); where != "" {
		p.addLabeledRows(addRow, "where: ", where, inner, approvalContextMaxLines)
	}
	if p.reason != "" {
		p.addLabeledRows(addRow, "reason: ", p.reason, inner, approvalContextMaxLines)
	}
	// What a "remember this" answer would authorize, in the same words the
	// gateway used when it decided the class was reusable.
	if p.details.GrantClass != "" {
		p.addLabeledRows(addRow, "remembering allows: ", p.details.GrantClass, inner, approvalContextMaxLines)
	}
	// The judge's own reasoning, when it ran and escalated: the person decides
	// faster reading one sentence than re-deriving it.
	if p.details.TriageRationale != "" {
		label := "triage: "
		if risk := p.details.TriageRisk; risk != "" {
			label = "triage (" + risk + " risk): "
		}
		p.addLabeledRows(addRow, label, p.details.TriageRationale, inner, approvalContextMaxLines)
	}
	if p.details.TriageUnavailable {
		for _, line := range WrapDisplay(approvalTriageUnavailableNotice, inner, approvalContextMaxLines) {
			addRow(approvalNoticeStyle.Render(line), line)
		}
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

// WrapDisplay wraps s into at most maxLines lines of at most width display
// columns each, breaking at spaces when one is available in the second half of
// the line and hard-breaking otherwise (paths and commands often have no spaces
// at all). Whitespace, including newlines, is normalized first so a multi-line
// payload cannot break the panel's box drawing. When the text does not fit in
// maxLines, the final line is middle-truncated: for a path or command the head
// and tail carry the meaning, so that beats dropping the tail outright.
func WrapDisplay(s string, width, maxLines int) []string {
	if width <= 0 || maxLines <= 0 {
		return nil
	}
	normalized := strings.Join(strings.Fields(s), " ")
	if normalized == "" {
		return nil
	}
	runes := []rune(normalized)
	var lines []string
	for len(runes) > 0 {
		if len(lines) == maxLines-1 {
			return append(lines, TruncateMiddle(string(runes), width))
		}
		taken, used := 0, 0
		for taken < len(runes) {
			w := runewidth.RuneWidth(runes[taken])
			if used+w > width {
				break
			}
			used += w
			taken++
		}
		if taken == len(runes) {
			return append(lines, string(runes))
		}
		if taken == 0 {
			// A single rune wider than the whole line: emit the marker rather
			// than loop forever.
			return append(lines, "…")
		}
		breakAt := taken
		for i := taken; i > taken/2; i-- {
			if runes[i-1] == ' ' {
				breakAt = i
				break
			}
		}
		lines = append(lines, strings.TrimRight(string(runes[:breakAt]), " "))
		runes = []rune(strings.TrimLeft(string(runes[breakAt:]), " "))
	}
	return lines
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
