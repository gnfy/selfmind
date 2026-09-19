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
	ID        string `json:"id"`
	Label     string `json:"label"`
	Decision  string `json:"decision"`
	Scope     string `json:"scope,omitempty"`
	GrantKey  string `json:"grant_key,omitempty"`
	RuleLabel string `json:"rule_label,omitempty"`
	Key       string `json:"key,omitempty"`
}

// buildApprovalDecisions renders one compact, server-authoritative answer set.
// Ordinary asks expose once / run-local reuse / deny. Sensitive asks expose
// once / deny. request_permissions exposes its exact run bundle / deny.
func buildApprovalDecisions(req tools.ToolApprovalRequest) []approvalDecisionOption {
	once := approvalDecisionOption{ID: "once", Label: "Yes, proceed", Decision: "approved", Key: "y"}
	deny := approvalDecisionOption{ID: "deny", Label: approvalDenyLabel(req.ToolName), Decision: "rejected", Key: "n"}

	switch strings.ToLower(strings.TrimSpace(req.DecisionPolicy)) {
	case tools.ApprovalDecisionPolicyOnceOnly:
		return []approvalDecisionOption{once, deny}
	case tools.ApprovalDecisionPolicyRunBundle:
		label := strings.TrimSpace(req.GrantClass)
		if label == "" {
			label = "the requested permissions"
		}
		return []approvalDecisionOption{{
			ID: "run_bundle", Label: "Yes, grant " + label + " for this run", Decision: "approved", Scope: "run", RuleLabel: label, Key: "y",
		}, deny}
	}

	options := []approvalDecisionOption{once}
	if len(req.RuleCandidates) == 1 {
		rule := req.RuleCandidates[0]
		options = append(options, approvalDecisionOption{
			ID: "rule:" + rule.Kind, Label: fmt.Sprintf("Yes, and don't ask again for %s in this run", rule.Label),
			Decision: "approved", Scope: "run", GrantKey: rule.Key, RuleLabel: rule.Label, Key: "r",
		})
	} else if exact := strings.TrimSpace(req.RunGrantClass); exact != "" {
		label := "Yes, and don't ask again for " + exact
		if approvalRunsCommand(req.ToolName) {
			label = "Yes, and don't ask again for this command in this run"
		}
		options = append(options, approvalDecisionOption{
			ID: "run_exact", Label: label, Decision: "approved", Scope: "run", RuleLabel: exact, Key: "r",
		})
	} else if class := strings.TrimSpace(req.GrantClass); class != "" {
		options = append(options, approvalDecisionOption{
			ID: "run", Label: "Yes, and don't ask again for " + class + " in this run", Decision: "approved", Scope: "run", RuleLabel: class, Key: "r",
		})
	}
	// The standing answer. A run-scoped reuse dies with the run, so the same
	// release workflow asked the same questions again the next morning; over one
	// week that was 478 approvals across 56 runs for 121 distinct classes. This
	// option is offered only when the floor minted a class, so the label always
	// names exactly what it widens.
	if class := strings.TrimSpace(req.GrantClass); class != "" {
		options = append(options, approvalDecisionOption{
			ID: "workspace", Label: "Yes, and stop asking for " + class + " in this workspace",
			Decision: "approved", Scope: "workspace", RuleLabel: class, Key: "a",
		})
	}
	return append(options, deny)
}

func approvalRunsCommand(toolName string) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "terminal", "execute_command", "shell", "execute_code", "verify", "watch_external":
		return true
	default:
		return false
	}
}

func approvalDenyLabel(toolName string) string {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "patch", "write_file":
		return "No, continue without making edits"
	case "request_permissions":
		return "No, continue without granting permissions"
	default:
		return "No, continue without running it"
	}
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

// Only advisory summaries use this small cap. Human evidence is bounded as a
// complete quotation bundle by tools.BoundApprovalEvidence.
const triageSummaryBytes = 1200

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
	return tools.RedactSensitive(trimmed)
}

// runIntentSnapshot captures approval evidence once at run start. Task text is
// advisory context. Human language is evidence for the model, never a keyword
// permission rule; actual control-plane decisions remain separate.
func runIntentSnapshot(req api.MessageRequest, task *control.Task, run *control.Run, workspace *control.Workspace) tools.RunIntentSnapshot {
	raw := triageIntentFromRequest(req.Content)
	snapshot := tools.RunIntentSnapshot{RawUserText: raw, Source: "direct", ModelAuthorization: true}
	if origin := strings.TrimSpace(req.Origin); origin != "" {
		snapshot.Source = "system:" + origin
	} else if req.ExecutionProfile != "" {
		snapshot.Source = "system:" + req.ExecutionProfile
	}
	if task != nil {
		snapshot.GoalSummary = truncate(tools.RedactSensitive(strings.TrimSpace(task.Title+"\n"+task.CurrentSummary)), triageSummaryBytes)
	}
	if run != nil {
		snapshot.WorkKey = run.WorkKey
	}
	if workspace != nil {
		snapshot.WorkspaceID = workspace.ID
	}

	return tools.BoundApprovalEvidence(snapshot)
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
