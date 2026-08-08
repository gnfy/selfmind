package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// Server-issued approval decisions (batch B1).
//
// Every surface used to invent its own answer set: the TUI hard-coded four
// options (once / task / person / deny) while Telegram offered two buttons
// (approve / reject) with no way to remember anything. The person's available
// choices therefore depended on which device they happened to be holding, and a
// new decision type could not be added without editing every client.
//
// The daemon now computes the list ONCE, from facts only it has — the grant class
// the floor was willing to mint, and the narrow rules this specific call could
// create — and publishes it with the ask. Clients render what they are given.
// A client that sends back a decision the daemon did not offer is refused at the
// execution layer (tools.approvalRuleByKey), so the list is an authorization
// contract, not a display hint.

// approvalDecisionOption is one answer a person may give. Key is the single-key
// shortcut a terminal or a conversational reply uses ("y", "p", …); it is stable
// per option KIND so muscle memory survives a changing option list.
type approvalDecisionOption struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Decision string `json:"decision"`
	Scope    string `json:"scope,omitempty"`
	GrantKey string `json:"grant_key,omitempty"`
	Key      string `json:"key,omitempty"`
}

// approvalDecisionShortcuts maps a rule kind to its shortcut letter. Prefix
// borrows codex's `p`; host and writable-root get `h` and `w`.
var approvalDecisionShortcuts = map[string]string{
	tools.ApprovalRuleKindCommandPrefix: "p",
	tools.ApprovalRuleKindNetworkHost:   "h",
	tools.ApprovalRuleKindPathRoot:      "w",
}

// approvalDecisionMaxOptions bounds the list. Past this an IM keypad and a
// terminal panel both become unreadable, and the tail options are the ones nobody
// picks: once/deny plus a few rules is the whole useful space.
const approvalDecisionMaxOptions = 6

// buildApprovalDecisions renders the ordered answer set for one ask. Order is
// deliberate: the narrowest answer first (run once), then the rules that end this
// specific question, then the broader class memory, then refusal last so it is
// never the default landing spot.
func buildApprovalDecisions(req tools.ToolApprovalRequest) []approvalDecisionOption {
	options := []approvalDecisionOption{{
		ID: "once", Label: "Yes, run it once", Decision: "approved", Key: "y",
	}}
	for _, rule := range req.RuleCandidates {
		if len(options) >= approvalDecisionMaxOptions-1 {
			break
		}
		options = append(options, approvalDecisionOption{
			ID:       "rule:" + rule.Kind,
			Label:    fmt.Sprintf("Yes, and don't ask again for %s", rule.Label),
			Decision: "approved",
			// A rule is remembered for the person: its whole value is surviving
			// the task it was granted in. The execution layer still bounds how
			// long a person-scope grant lasts (approvalGrantExpiry).
			Scope:    "person",
			GrantKey: rule.Key,
			Key:      approvalDecisionShortcuts[rule.Kind],
		})
	}
	// An unclassifiable script may still be approved as a byte-identical action
	// for this live run. This is explicit, in-memory, and cannot become task or
	// person authority.
	if strings.TrimSpace(req.GrantClass) == "" && strings.TrimSpace(req.RunGrantClass) != "" && len(options) < approvalDecisionMaxOptions-1 {
		options = append(options, approvalDecisionOption{
			ID: "run_exact", Label: "Yes, and allow " + req.RunGrantClass, Decision: "approved", Scope: "run", Key: "r",
		})
	}
	// Class memory is offered only when the grant floor actually minted a class.
	// Offering it otherwise promises memory that would be silently discarded —
	// the exact dishonesty tools.ToolApprovalRequest.GrantClass exists to prevent.
	if strings.TrimSpace(req.GrantClass) != "" && len(options) < approvalDecisionMaxOptions-1 {
		options = append(options,
			approvalDecisionOption{ID: "run", Label: "Yes, and allow " + req.GrantClass + " for this run", Decision: "approved", Scope: "run", Key: "r"},
		)
	}
	if strings.TrimSpace(req.GrantClass) != "" && len(options) < approvalDecisionMaxOptions-1 {
		options = append(options,
			approvalDecisionOption{ID: "task", Label: "Yes, and allow " + req.GrantClass + " for this task", Decision: "approved", Scope: "task", Key: "t"},
		)
		if len(options) < approvalDecisionMaxOptions-1 {
			options = append(options,
				approvalDecisionOption{ID: "person", Label: "Yes, always allow " + req.GrantClass, Decision: "approved", Scope: "person", Key: "a"},
			)
		}
	}
	return append(options, approvalDecisionOption{
		ID: "deny", Label: "No, and tell the agent what to do instead", Decision: "rejected", Key: "n",
	})
}

// decodeApprovalDecisions reads the options back off a stored row's payload. A row
// written before this batch carries none, and callers fall back to their own
// defaults — the same behavior as before, never an empty option list.
func decodeApprovalDecisions(payload []byte) []approvalDecisionOption {
	if len(payload) == 0 {
		return nil
	}
	var envelope struct {
		Decisions []approvalDecisionOption `json:"decisions"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil
	}
	return envelope.Decisions
}

// approvalOptionByShortcut resolves a conversational one-letter answer against the
// options this ask actually offered. It is how IM keeps parity with the TUI: the
// letters come from the same server-issued list, so "yp" means whatever `p` meant
// on THIS ask and nothing else.
func approvalOptionByShortcut(options []approvalDecisionOption, letter string) (approvalDecisionOption, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(letter))
	if trimmed == "" {
		return approvalDecisionOption{}, false
	}
	for _, option := range options {
		if option.Key == trimmed {
			return option, true
		}
	}
	return approvalDecisionOption{}, false
}

// triageIntentMaxChars bounds the instruction handed to the judge. The
// authorization question needs what the person asked for, not the whole message.
const triageIntentMaxChars = 1200

// triageIntentFromRequest prepares the person's own words for the triage judge:
// gateway decoration is already absent from req.Content, and the text is
// redacted and bounded because it becomes part of a prompt sent to a cheap role
// model. It is evidence, never an instruction — the judge prompt delimits it and
// declares it untrusted.
func triageIntentFromRequest(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	return truncate(tools.RedactSensitive(trimmed), triageIntentMaxChars)
}

// runIntentSnapshot captures approval evidence once at run start. Task text is
// advisory context; deterministic user allow/deny signals remain separate so a
// model-generated summary can never silently become authorization.
func runIntentSnapshot(req api.MessageRequest, task *control.Task, run *control.Run, workspace *control.Workspace) tools.RunIntentSnapshot {
	raw := triageIntentFromRequest(req.Content)
	snapshot := tools.RunIntentSnapshot{RawUserText: raw, Source: "direct"}
	if origin := strings.TrimSpace(req.Origin); origin != "" {
		snapshot.Source = "system:" + origin
	} else if req.ExecutionProfile != "" {
		snapshot.Source = "system:" + req.ExecutionProfile
	}
	if task != nil {
		snapshot.GoalSummary = truncate(tools.RedactSensitive(strings.TrimSpace(task.Title+"\n"+task.CurrentSummary)), triageIntentMaxChars)
	}
	if run != nil {
		snapshot.WorkKey = run.WorkKey
	}
	if workspace != nil {
		snapshot.WorkspaceID = workspace.ID
	}
	compact := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "。.!！?？ \t\r\n"))
	switch compact {
	case "continue", "resume", "keep going", "go on", "继续", "开始执行", "执行吧", "请执行", "可以", "同意", "确认执行":
		if snapshot.UserAuthored() {
			snapshot.Source = "continuation"
			snapshot.ExplicitAllow = []string{"continue-current-task"}
		}
	}
	for _, marker := range []string{"不要", "禁止", "别执行", "do not", "don't", "must not", "never"} {
		if strings.Contains(compact, marker) {
			snapshot.ExplicitDeny = append(snapshot.ExplicitDeny, marker)
		}
	}
	return snapshot
}

// fallbackApprovalReason prefers the person's own refusal words over a generic
// "rejected", so the model receives the guidance rather than a bare no.
func fallbackApprovalReason(note, alternative string) string {
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		return trimmed
	}
	return alternative
}

// approvalOptionLines renders the option list for a text surface (IM push,
// /approvals detail). It is the same list the panel draws, so a person answering
// on WeChat is choosing from the same menu as one answering in the terminal.
func approvalOptionLines(options []approvalDecisionOption) string {
	if len(options) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, option := range options {
		if option.Key == "" {
			continue
		}
		fmt.Fprintf(&sb, "  %s = %s\n", option.Key, option.Label)
	}
	return sb.String()
}
