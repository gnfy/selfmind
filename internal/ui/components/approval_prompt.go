package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// ApprovalOption is one selectable answer in an ApprovalPrompt panel. Decision
// and Scope mirror the gateway approval-respond contract
// (api.ApprovalRespondRequest): Decision is "approved"|"rejected"; Scope is the
// grant memory scope — "" (once) or the daemon-issued reusable scope.
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

// DefaultApprovalOptions is the compatibility answer set used with an older
// daemon that did not publish choices. It is intentionally once-only: a client
// must never invent durable authority that the daemon did not offer.
func DefaultApprovalOptions() []ApprovalOption {
	return []ApprovalOption{
		{Label: "Yes, proceed", Key: "y", Decision: "approved", Scope: ""},
		{Label: "No, continue without it", Key: "n", Decision: "rejected", Scope: ""},
	}
}

func defaultApprovalOptionsForTool(tool string) []ApprovalOption {
	options := DefaultApprovalOptions()
	switch strings.TrimSpace(tool) {
	case "terminal", "execute_command", "shell", "execute_code", "verify", "watch_external":
		options[1].Label = "No, continue without running it"
	case "patch", "write_file":
		options[1].Label = "No, continue without making edits"
	case "request_permissions":
		options[1].Label = "No, continue without granting permissions"
	}
	return options
}

// ApprovalPrompt is a Codex-style selectable approval list rendered in the
// TUI's active region while a run is blocked on a tool approval. It is a passive
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
	// Parked means the original run released its resources. The request remains
	// answerable, and a decision will start a continuation rather than wake a
	// live waiter.
	Parked bool
	// Environment is the bound environment snapshot id; Cwd is the working root
	// the operation would run in (the scope's root, never the daemon cwd).
	Environment string
	Cwd         string
	// ChangeSummary is a content-free size line ("2 files +48/-12").
	ChangeSummary string
	// GrantClass names what a run-local reuse choice would authorize, or "" when the
	// daemon did not publish one. It is rendered, not acted on: whether a grant
	// is actually persisted is the grant floor's decision, so the panel must not
	// infer an option set from it.
	GrantClass string
	// Containment is the daemon's filesystem/network/credential assessment.
	// It helps the person distinguish an isolated read from a host escape.
	Containment string
	// Execute-code approvals carry only a bounded, redacted preview plus
	// metadata. The full source remains execution input and is never persisted
	// or rendered by the approval surface.
	CodePreview string
	CodeSHA256  string
	CodeLines   int
	CodeBytes   int
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
	// back to the built-in once/deny pair so an older daemon still renders a usable panel;
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
	d.Containment = strings.TrimSpace(d.Containment)
	d.CodePreview = strings.TrimSpace(d.CodePreview)
	d.CodeSHA256 = strings.TrimSpace(d.CodeSHA256)
	options := d.Options
	if len(options) == 0 {
		options = defaultApprovalOptionsForTool(d.Tool)
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

// SetParked updates the lifecycle explanation without changing the action or
// server-issued decisions.
func (p *ApprovalPrompt) SetParked(parked bool) {
	if p != nil {
		p.details.Parked = parked
	}
}

// IsParked reports whether answering starts a continuation instead of waking
// the original run.
func (p *ApprovalPrompt) IsParked() bool { return p != nil && p.details.Parked }

// HandleKey processes one key press. It returns the chosen option when the key
// resolves the prompt (Enter on the highlighted row, or a shortcut key), and
// nil otherwise. Up/Down and j/k move the selector. The controller owns Esc and
// Ctrl+C because both must send an explicit rejection before closing the panel.
// All other keys are ignored.
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
	approvalTitleStyle  = lipgloss.NewStyle().Bold(true)
	approvalToolStyle   = lipgloss.NewStyle().Bold(true)
	approvalDimStyle    = lipgloss.NewStyle().Faint(true)
	approvalSelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	approvalNoticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	approvalTargetStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
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
func (p *ApprovalPrompt) addLabeledRows(addRow func(string), label, value string, inner, maxLines int) {
	labelW := runewidth.StringWidth(label)
	lines := WrapDisplay(value, maxInt(8, inner-labelW), maxLines)
	if len(lines) == 0 {
		return
	}
	addRow(approvalDimStyle.Render(label) + lines[0])
	indent := strings.Repeat(" ", labelW)
	for _, line := range lines[1:] {
		addRow(indent + line)
	}
}

// approvalPanelMaxWidth caps the list so it stays readable in a wide terminal.
const approvalPanelMaxWidth = 76

func (p *ApprovalPrompt) question() string {
	switch p.tool {
	case "terminal", "execute_command", "shell", "execute_code", "verify", "watch_external":
		return "Would you like to run the following command?"
	case "patch", "write_file":
		return "Would you like to make the following edits?"
	case "request_permissions":
		return "Would you like to grant these permissions?"
	default:
		return "Would you like to allow this tool call?"
	}
}

func (p *ApprovalPrompt) targetPrefix() string {
	switch p.tool {
	case "terminal", "execute_command", "shell", "execute_code", "verify", "watch_external":
		return "$ "
	default:
		return ""
	}
}

// View renders the same information hierarchy as Codex's approval overlay:
// question, inspectable action/context, server-issued decisions, then the
// explicit confirm/cancel footer. Proposed rules wrap instead of being
// middle-truncated because that text is the authority the person is approving.
func (p *ApprovalPrompt) View(width int) string {
	panelW := width
	if panelW > approvalPanelMaxWidth {
		panelW = approvalPanelMaxWidth
	}
	if panelW < 12 {
		panelW = 12
	}
	inner := panelW

	type row struct {
		text string // styled content
	}
	var rows []row
	addRow := func(styled string) {
		rows = append(rows, row{text: styled})
	}

	for _, line := range WrapDisplay(p.question(), inner, 2) {
		addRow(approvalTitleStyle.Render(line))
	}
	if p.details.Parked {
		for _, line := range WrapDisplay("The task is parked; your answer will resume it.", inner, 2) {
			addRow(approvalNoticeStyle.Render(line))
		}
	}
	addRow("")

	// Keep the exact command/path visible. A middle ellipsis here would hide the
	// part the person is being asked to authorize.
	if p.target != "" {
		prefix := p.targetPrefix()
		lines := WrapDisplay(p.target, maxInt(8, inner-runewidth.StringWidth(prefix)), approvalTargetMaxLines)
		for i, line := range lines {
			styled := approvalTargetStyle.Render(line)
			if i == 0 {
				styled = approvalDimStyle.Render(prefix) + styled
			}
			addRow(styled)
		}
	} else if p.tool != "" {
		addRow(approvalToolStyle.Render(p.tool))
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
	if p.details.Containment != "" {
		p.addLabeledRows(addRow, "access: ", p.details.Containment, inner, approvalContextMaxLines)
	}
	if p.reason != "" {
		p.addLabeledRows(addRow, "reason: ", p.reason, inner, approvalContextMaxLines)
	}
	if p.details.CodePreview != "" {
		p.addLabeledRows(addRow, "code: ", p.details.CodePreview, inner, 4)
		var metadata []string
		if p.details.CodeLines > 0 {
			metadata = append(metadata, fmt.Sprintf("%d lines", p.details.CodeLines))
		}
		if p.details.CodeBytes > 0 {
			metadata = append(metadata, fmt.Sprintf("%d bytes", p.details.CodeBytes))
		}
		if p.details.CodeSHA256 != "" {
			digest := p.details.CodeSHA256
			if len(digest) > 12 {
				digest = digest[:12]
			}
			metadata = append(metadata, "sha256 "+digest)
		}
		if len(metadata) > 0 {
			p.addLabeledRows(addRow, "script: ", strings.Join(metadata, ", "), inner, 1)
		}
	}
	// Older daemons publish only the coarse class. New daemons put the exact
	// proposed authority on the option itself; do not show both and imply that a
	// narrow rule also grants the broader class.
	hasExplicitRule := false
	for _, option := range p.options {
		if strings.TrimSpace(option.RuleLabel) != "" {
			hasExplicitRule = true
			break
		}
	}
	if p.details.GrantClass != "" && !hasExplicitRule {
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
			addRow(approvalNoticeStyle.Render(line))
		}
	}
	addRow("")

	for i, opt := range p.options {
		marker := "  "
		style := lipgloss.NewStyle()
		if i == p.cursor {
			marker = "❯ "
			style = approvalSelStyle
		}
		hint := ""
		if opt.Key != "" {
			hint = "(" + opt.Key + ") "
		}
		lineWidth := maxInt(8, inner-runewidth.StringWidth(marker)-runewidth.StringWidth(hint))
		// Unlike descriptive context, a proposed rule must never be abbreviated:
		// this is the exact authority the person is confirming.
		maxRuleLines := maxInt(1, runewidth.StringWidth(strings.Join(strings.Fields(opt.Label), " "))/lineWidth+2)
		lines := WrapDisplay(opt.Label, lineWidth, maxRuleLines)
		if len(lines) == 0 {
			lines = []string{"decision"}
		}
		for lineIndex, line := range lines {
			if lineIndex == 0 {
				styled := style.Render(marker) + approvalDimStyle.Render(hint) + style.Render(line)
				addRow(styled)
				continue
			}
			indent := strings.Repeat(" ", runewidth.StringWidth(marker)+runewidth.StringWidth(hint))
			addRow(indent + style.Render(line))
		}
	}
	footer := "↑/↓ to move · enter to confirm · esc to cancel"
	footer = TruncateMiddle(footer, inner)
	addRow("")
	addRow(approvalDimStyle.Render(footer))

	var sb strings.Builder
	for i, r := range rows {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(r.text)
	}
	return sb.String()
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
